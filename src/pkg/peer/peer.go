package peer

import (
	"doozer/consensus"
	"doozer/gc"
	"doozer/member"
	"doozer/server"
	"doozer/store"
	"doozer/web"
	"github.com/ha/doozer"
	"net"
	"os"
	"time"
	"log"
)

const (
	alpha     = 50
	maxUDPLen = 3000
)

const calDir = "/ctl/cal"

var calGlob = store.MustCompileGlob(calDir + "/*")


type proposer struct {
	seqns chan int64
	props chan *consensus.Prop
	st    *store.Store
}


func (p *proposer) Propose(v []byte) (e store.Event) {
	for e.Mut != string(v) {
		n := <-p.seqns
		w, err := p.st.Wait(n)
		if err != nil {
			panic(err) // can't happen
		}
		p.props <- &consensus.Prop{n, v}
		e = <-w
	}
	return
}


func Main(clusterName, self, baddr string, cl *doozer.Client, udpConn net.PacketConn, listener, webListener net.Listener, pulseInterval, fillDelay, kickTimeout int64) {
	listenAddr := listener.Addr().String()

	var activateSeqn int64
	useSelf := make(chan bool, 1)

	st := store.New()
	pr := &proposer{
		seqns: make(chan int64, alpha),
		props: make(chan *consensus.Prop),
		st:    st,
	}

	calSrv := func() {
		go gc.Pulse(self, st.Seqns, pr, pulseInterval)
		go gc.Clean(st, 360000, time.Tick(1e9))
	}

	if cl == nil { // we are the only node in a new cluster
		set(st, "/ctl/name", clusterName, store.Missing)
		set(st, "/ctl/node/"+self+"/addr", listenAddr, store.Missing)
		set(st, "/ctl/node/"+self+"/hostname", os.Getenv("HOSTNAME"), store.Missing)
		set(st, "/ctl/node/"+self+"/version", Version, store.Missing)
		set(st, "/ctl/cal/0", self, store.Missing)
		calSrv()
		close(useSelf)
	} else {
		setC(cl, "/ctl/node/"+self+"/addr", listenAddr, store.Clobber)
		setC(cl, "/ctl/node/"+self+"/hostname", os.Getenv("HOSTNAME"), store.Clobber)
		setC(cl, "/ctl/node/"+self+"/version", Version, store.Clobber)

		rev, err := cl.Rev()
		if err != nil {
			panic(err)
		}

		walk, err := cl.Walk("/**", &rev, nil, nil)
		if err != nil {
			panic(err)
		}

		watch, err := cl.Watch("/**", rev+1)
		if err != nil {
			panic(err)
		}

		go follow(st.Ops, watch.C)
		follow(st.Ops, walk.C)
		st.Flush()
		ch, err := st.Wait(rev + 1)
		if err == nil {
			<-ch
		}

		go func() {
			activateSeqn = activate(st, self, cl)
			calSrv()
			advanceUntil(cl, st.Seqns, activateSeqn+alpha)
			err := watch.Cancel()
			if err != nil {
				panic(err)
			}
			close(useSelf)
			if baddr != "" {
				b := doozer.New("<boot>", baddr)
				setC(
					b,
					"/ctl/ns/"+clusterName+"/"+self,
					listenAddr,
					store.Missing,
				)
			}
		}()
	}

	start := <-st.Seqns
	cmw := st.Watch(store.Any)
	in := make(chan consensus.Packet, 50)
	out := make(chan consensus.Packet, 50)

	consensus.NewManager(self, start, alpha, in, out, st.Ops, pr.seqns, pr.props, cmw, fillDelay, st)

	if cl == nil {
		// Skip ahead alpha steps so that the registrar can provide a
		// meaningful cluster.
		for i := start + 1; i < start+alpha+1; i++ {
			st.Ops <- store.Op{i, store.Nop}
		}
	}

	shun := make(chan string, 3) // sufficient for a cluster of 7

	go member.Clean(shun, st, pr)

	sv := &server.Server{listenAddr, st, pr, self, alpha}

	go sv.Serve(listener, useSelf)

	if webListener != nil {
		web.Store = st
		web.ClusterName = clusterName
		go web.Serve(webListener)
	}

	go func() {
		for p := range out {
			addr, err := net.ResolveUDPAddr(p.Addr)
			if err != nil {
				log.Println(err)
				continue
			}
			n, err := udpConn.WriteTo(p.Data, addr)
			if err != nil {
				log.Println(err)
				continue
			}
			if n != len(p.Data) {
				log.Println("packet len too long:", len(p.Data))
				continue
			}
		}
	}()

	lv := liveness{
		timeout: kickTimeout,
		ival:    kickTimeout / 2,
		times:   make(map[string]int64),
		self:    self,
		shun:    shun,
	}
	for {
		t := time.Nanoseconds()

		buf := make([]byte, maxUDPLen)
		n, addr, err := udpConn.ReadFrom(buf)
		if err == os.EINVAL {
			return
		}
		if err != nil {
			log.Println(err)
			continue
		}

		buf = buf[:n]

		// Update liveness time stamp for this addr
		lv.times[addr.String()] = t
		lv.check(t)

		in <- consensus.Packet{addr.String(), buf}
	}
}


func activate(st *store.Store, self string, c *doozer.Client) int64 {
	w := store.NewWatch(st, calGlob)

	for _, base := range store.Getdir(st, calDir) {
		p := calDir + "/" + base
		v, rev := st.Get(p)
		if rev != store.Dir && v[0] == "" {
			seqn, err := c.Set(p, rev, []byte(self))
			if err != nil {
				log.Println(err)
				continue
			}

			w.Stop()
			return seqn
		}
	}

	for ev := range w.C {
		// TODO ev.IsEmpty()
		if ev.IsSet() && ev.Body == "" {
			seqn, err := c.Set(ev.Path, ev.Rev, []byte(self))
			if err != nil {
				log.Println(err)
				continue
			}
			w.Stop()
			return seqn
		}
	}

	return 0
}

func advanceUntil(cl *doozer.Client, ver <-chan int64, done int64) {
	for <-ver < done {
		cl.Nop()
	}
}

func set(st *store.Store, path, body string, rev int64) {
	mut := store.MustEncodeSet(path, body, rev)
	st.Ops <- store.Op{1 + <-st.Seqns, mut}
}

func setC(cl *doozer.Client, path, body string, rev int64) {
	_, err := cl.Set(path, rev, []byte(body))
	if err != nil {
		panic(err)
	}
}

func follow(ops chan<- store.Op, ch <-chan *doozer.Event) {
	for ev := range ch {
		// store.Clobber is okay here because the event
		// has already passed through another store
		mut := store.MustEncodeSet(ev.Path, string(ev.Body), store.Clobber)
		ops <- store.Op{ev.Rev, mut}
	}
}
