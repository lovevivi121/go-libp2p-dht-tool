package main

import (
	"context"
	"fmt"
	_ "github.com/anacrolix/envpprof"
	"github.com/anacrolix/tagflag"
	"github.com/ipfs/go-cid"
	ipfsGoLog "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kbucket"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/peterh/liner"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

func main() {
	err := errMain()
	if err != nil {
		log.Fatal(err)
	}
}

func errMain() error {
	log.SetFlags(log.Flags() | log.Llongfile)
	var cmd struct {
		Passive bool `help:"start DHT node in client-only mode"`
	}
	tagflag.Parse(&cmd)
	host, err := libp2p.New()
	if err != nil {
		return fmt.Errorf("error creating host: %s", err)
	}
	defer host.Close()
	d, err := dht.New(context.Background(), host, dht.Mode(dht.ModeServer))
	if err != nil {
		return fmt.Errorf("error creating dht node: %s", err)
	}
	defer d.Close()
	return interactiveLoop(d, host)
}

type commandHandler interface {
	Do(context.Context, *dht.IpfsDHT, host.Host, []string)
	ArgHelp() string
}

type commandFunc struct {
	f       func(context.Context, *dht.IpfsDHT, host.Host, []string)
	argHelp string
}

func (me commandFunc) Do(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
	me.f(ctx, d, h, args)
}

func (me commandFunc) ArgHelp() string { return me.argHelp }

type nullaryFunc func(context.Context, *dht.IpfsDHT, host.Host)

func (me nullaryFunc) Do(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
	if len(args) > 0 {
		log.Print("command does not take arguments")
		return
	}
	me(ctx, d, h)
}

func (me nullaryFunc) ArgHelp() string { return "" }

var commandOutputWriter = os.Stdout

var allCommands map[string]commandHandler

func init() {
	allCommands = map[string]commandHandler{
		"add_bootstrap_nodes": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			for _, bna := range dht.DefaultBootstrapPeers {
				addr, last := multiaddr.SplitLast(bna)
				p, err := peer.Decode(last.Value())
				if err != nil {
					log.Printf("can't decode %q: %v", last, err)
					continue
				}
				d.Host().Peerstore().AddAddrs(p, []multiaddr.Multiaddr{addr}, time.Hour)
				d.RoutingTable().UpdateLastSuccessfulOutboundQueryAt(p, time.Now().Add(1*time.Hour))
			}
		}),
		"connect_bootstrap_nodes": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			bootstrapNodeAddrs := dht.DefaultBootstrapPeers
			numConnected := connectToBootstrapNodes(ctx, h, bootstrapNodeAddrs)
			if numConnected == 0 {
				log.Print("failed to connect to any bootstrap nodes")
			} else {
				log.Printf("connected to %d/%d bootstrap nodes", numConnected, len(bootstrapNodeAddrs))
			}
		}),
		//"bootstrap_once": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
		//	cfg := dht.DefaultBootstrapConfig
		//	//cfg.Timeout = time.Minute
		//	err := d.BootstrapOnce(ctx, cfg)
		//	if err != nil {
		//		fmt.Fprintf(commandOutputWriter, "%v\n", err)
		//	}
		//}),
		"bootstrap_self": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			fmt.Fprintf(commandOutputWriter, "%v\n", d.Bootstrap(ctx))
		}),
		"bootstrap_random": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			fmt.Fprintf(commandOutputWriter, "%v\n", d.Bootstrap(ctx))
		}),
		"select_indefinitely": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			<-ctx.Done()
		}),
		"print_routing_table": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			doPrintRoutingTable(os.Stdout, d)
		}),
		"print_self_id": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			fmt.Printf("%s (%x)\n", d.PeerID().Pretty(), d.PeerKey())
		}),
		"ping": commandFunc{
			func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
				id, err := peer.Decode(args[0])
				if err != nil {
					log.Printf("can't parse peer id: %v", err)
					return
				}
				started := time.Now()
				err = d.Ping(ctx, id)
				fmt.Fprintf(commandOutputWriter, "ping result after %v: %v\n", time.Since(started), err)
			},
			"<peer_id>",
		},
		"find_peer": commandFunc{
			func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
				pid, err := peer.Decode(args[0])
				if err != nil {
					fmt.Fprintf(commandOutputWriter, "error decoding peer id: %v\n", err)
					return
				}
				pi, err := d.FindPeer(ctx, pid)
				if err != nil {
					fmt.Fprintf(commandOutputWriter, "error finding peer: %v\n", err)
					return
				}
				fmt.Fprintf(commandOutputWriter, "%q has addresses:\n", pid)
				for _, a := range pi.Addrs {
					fmt.Fprintln(commandOutputWriter, a)
				}
			},
			"<peer_id>",
		},
		"provide": commandFunc{
			func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
				key, err := cid.Decode(args[0])
				if err != nil {
					fmt.Fprintf(commandOutputWriter, "error decoding %q: %v\n", args[0], err)
					return
				}
				broadcast := true
				if len(args) >= 2 {
					var err error
					broadcast, err = strconv.ParseBool(args[1])
					if err != nil {
						fmt.Fprintf(commandOutputWriter, "error parsing bool from %q: %v\n", args[1], err)
						return
					}
				}
				d.Provide(ctx, key, broadcast)
			}, "<key>",
		},
		"find_providers": commandFunc{
			func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
				key, err := cid.Decode(args[0])
				if err != nil {
					fmt.Fprintf(commandOutputWriter, "error decoding %q: %v\n", args[0], err)
					return
				}
				count := math.MaxInt32
				if len(args) >= 2 {
					count64, err := strconv.ParseInt(args[1], 0, 0)
					if err != nil {
						fmt.Fprintf(commandOutputWriter, "error parsing count: %v\n", err)
						return
					}
					count = int(count64)
				}
				for pi := range d.FindProvidersAsync(ctx, key, count) {
					fmt.Fprintln(commandOutputWriter, pi)
				}
			},
			"<key> [num_of_providers]",
		},
		"set_ipfs_log_level": commandFunc{
			func(ctx context.Context, d *dht.IpfsDHT, h host.Host, args []string) {
				err := ipfsGoLog.SetLogLevel(args[0], args[1])
				if err != nil {
					fmt.Fprintln(commandOutputWriter, err)
				}
			},
			"<component> <level>"},
		"help": nullaryFunc(func(ctx context.Context, d *dht.IpfsDHT, h host.Host) {
			fmt.Fprintln(commandOutputWriter, "Commands:")
			tw := tabwriter.NewWriter(commandOutputWriter, 0, 0, 2, ' ', 0)
			var keys []string
			for k := range allCommands {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(tw, "\t%s\t%s\n", k, allCommands[k].ArgHelp())
			}
			tw.Flush()
		}),
	}
}

var historyPath = ".libp2p-dht-tool-history"

func readHistory(s *liner.State) (int, error) {
	f, err := os.Open(historyPath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return s.ReadHistory(f)
}

func writeHistory(s *liner.State) (int, error) {
	f, err := os.OpenFile(historyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	num, err := s.WriteHistory(f)
	if err != nil {
		return num, err
	}
	return num, f.Close()
}

func interactiveLoop(d *dht.IpfsDHT, h host.Host) error {
	s := liner.NewLiner()
	if _, err := readHistory(s); err != nil {
		log.Printf("error reading history: %v", err)
	}
	defer func() {
		if _, err := writeHistory(s); err != nil {
			log.Printf("error writing history: %v", err)
		}
	}()
	s.SetTabCompletionStyle(liner.TabPrints)
	s.SetCompleter(func(line string) (ret []string) {
		for c := range allCommands {
			if strings.HasPrefix(c, line) {
				ret = append(ret, c)
			}
		}
		return
	})
	defer s.Close()
	for {
		p, err := s.Prompt("> ")
		if err == io.EOF {
			return nil
		}
		if err != nil {
			panic(err)
		}
		if handleInput(p, d, h) {
			s.AppendHistory(p)
		}
	}
}

func handleInput(input string, d *dht.IpfsDHT, h host.Host) (addHistory bool) {
	inputFields := strings.Fields(input)
	intChan := make(chan os.Signal, 1)
	signal.Notify(intChan, os.Interrupt)
	defer signal.Stop(intChan)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-intChan:
			cancel()
		case <-ctx.Done():
		}
	}()
	if len(inputFields) == 0 {
		return false
	}
	if handler, ok := allCommands[inputFields[0]]; ok {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			addHistory = true
			fmt.Fprintf(commandOutputWriter, "panic handling command: %v\n", r)
			debug.PrintStack()
		}()
		handler.Do(ctx, d, h, inputFields[1:])
		return true
	}
	fmt.Fprintf(commandOutputWriter, "unknown command: %q\n", input)
	return false
}

func doPrintRoutingTable(w io.Writer, d *dht.IpfsDHT) {
	for i, p := range d.RoutingTable().ListPeers() {

		fmt.Fprintf(w, "%3d %3d %x %v %v\n",
			i,
			kbucket.CommonPrefixLen(kbucket.ConvertPeerID(p), kbucket.ConvertPeerID(d.PeerID())),
			kbucket.ConvertPeerID(p),
			p.String(),
			d.Host().Network().Connectedness(p),
		)

	}
}

func connectToBootstrapNodes(ctx context.Context, h host.Host, mas []multiaddr.Multiaddr) (numConnected int32) {
	var wg sync.WaitGroup
	for _, ma := range mas {
		wg.Add(1)
		go func(ma multiaddr.Multiaddr) {
			pi, err := peer.AddrInfoFromP2pAddr(ma)
			if err != nil {
				panic(err)
			}
			defer wg.Done()
			err = h.Connect(ctx, *pi)
			if err != nil {
				log.Printf("error connecting to bootstrap node %q: %v", ma, err)
			} else {
				atomic.AddInt32(&numConnected, 1)
			}
		}(ma)
	}
	wg.Wait()
	return
}

//func setupMetrics(d *dht.IpfsDHT) error {
//	go func() {
//		mux := http.NewServeMux()
//		mux.Handle("/metrics", promhttp.Handler())
//		panic(http.ListenAndServe(os.Getenv("PROM_ENDPOINT"), mux))
//	}()
//	return nil
//}
