// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/howeyc/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/prometheus/statsd_exporter/pkg/mapper"
)

func init() {
	prometheus.MustRegister(version.NewCollector("statsd_exporter"))
}

func serveHTTP(listenAddress, metricsEndpoint string) {
	//lint:ignore SA1019 prometheus.Handler() is deprecated.
	http.Handle(metricsEndpoint, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>StatsD Exporter</title></head>
			<body>
			<h1>StatsD Exporter</h1>
			<p><a href="` + metricsEndpoint + `">Metrics</a></p>
			</body>
			</html>`))
	})
	log.Fatal(http.ListenAndServe(listenAddress, nil))
}

func ipPortFromString(addr string) (*net.IPAddr, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatal("Bad StatsD listening address", addr)
	}

	if host == "" {
		host = "0.0.0.0"
	}
	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		log.Fatalf("Unable to resolve %s: %s", host, err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		log.Fatalf("Bad port %s: %s", portStr, err)
	}

	return ip, port
}

func udpAddrFromString(addr string) *net.UDPAddr {
	ip, port := ipPortFromString(addr)
	return &net.UDPAddr{
		IP:   ip.IP,
		Port: port,
		Zone: ip.Zone,
	}
}

func tcpAddrFromString(addr string) *net.TCPAddr {
	ip, port := ipPortFromString(addr)
	return &net.TCPAddr{
		IP:   ip.IP,
		Port: port,
		Zone: ip.Zone,
	}
}

func watchConfig(fileName string, mapper *mapper.MetricMapper, cacheSize int) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	err = watcher.WatchFlags(fileName, fsnotify.FSN_MODIFY)
	if err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case ev := <-watcher.Event:
			log.Infof("Config file changed (%s), attempting reload", ev)
			err = mapper.InitFromFile(fileName, cacheSize)
			if err != nil {
				log.Errorln("Error reloading config:", err)
				configLoads.WithLabelValues("failure").Inc()
			} else {
				log.Infoln("Config reloaded successfully")
				configLoads.WithLabelValues("success").Inc()
			}
			// Re-add the file watcher since it can get lost on some changes. E.g.
			// saving a file with vim results in a RENAME-MODIFY-DELETE event
			// sequence, after which the newly written file is no longer watched.
			_ = watcher.WatchFlags(fileName, fsnotify.FSN_MODIFY)
		case err := <-watcher.Error:
			log.Errorln("Error watching config:", err)
		}
	}
}

func dumpFSM(mapper *mapper.MetricMapper, dumpFilename string) error {
	f, err := os.Create(dumpFilename)
	if err != nil {
		return err
	}
	log.Infoln("Start dumping FSM to", dumpFilename)
	w := bufio.NewWriter(f)
	mapper.FSM.DumpFSM(w)
	w.Flush()
	f.Close()
	log.Infoln("Finish dumping FSM")
	return nil
}

func watchUDPBuffers(lastQueued int, lastDropped int, lastQueued6 int, lastDropped6 int) {
	myPid := strconv.Itoa(os.Getpid())

	queuedUDP, droppedUDP := parseProcfsNetFile("/proc/" + myPid + "/net/udp")
	label := "udp"

	diff := queuedUDP - lastQueued
	if diff < 0 {
		log.Info("Queue count went negative! Abandoning UDP buffer parsing")
		return
	}
	udpBufferQueued.WithLabelValues(label).Inc()

	diff = droppedUDP - lastDropped
	if diff < 0 {
		log.Info("Dropped count went negative! Abandoning UDP buffer parsing")
		return
	}
	udpBufferDropped.WithLabelValues(label).Inc()

	queuedUDP6, droppedUDP6 := parseProcfsNetFile("/proc/" + myPid + "/net/udp6")
	label = "udp6"

	diff = queuedUDP6 - lastQueued6
	if diff < 0 {
		log.Info("Queue count went negative! Abandoning UDP buffer parsing")
		return
	}
	udpBufferQueued.WithLabelValues(label).Inc()

	diff = droppedUDP6 - lastDropped6
	if diff < 0 {
		log.Info("Dropped count went negative! Abandoning UDP buffer parsing")
		return
	}
	udpBufferDropped.WithLabelValues(label).Inc()

	time.Sleep(10 * time.Second)
	watchUDPDrops(queuedUDP, droppedUDP, queuedUDP6, droppedUDP6)
}

func parseProcfsNetFile(filename string) (int, int) {
	f, err := os.Open(filename)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	queued := 0
	dropped := 0
	s := bufio.NewScanner(f)
	for n := 0; s.Scan(); n++ {
		// Skip the header lines.
		if n < 1 {
			continue
		}

		fields := strings.Fields(s.Text())

		queuedLine, err := strconv.Atoi(strings.Split(fields[4], ":")[1])
		queued = queued + queuedLine
		if err != nil {
			log.Info("Unable to parse queued UDP buffers:", err)
			return 0, 0
		}

		droppedLine, err := strconv.Atoi(fields[12])
		dropped = dropped + droppedLine
		if err != nil {
			log.Info("Unable to parse dropped UDP buffers:", err)
			return 0, 0
		}
	}

	return queued, dropped
}

func main() {
	var (
		listenAddress        = kingpin.Flag("web.listen-address", "The address on which to expose the web interface and generated Prometheus metrics.").Default(":9102").String()
		metricsEndpoint      = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		statsdListenUDP      = kingpin.Flag("statsd.listen-udp", "The UDP address on which to receive statsd metric lines. \"\" disables it.").Default(":9125").String()
		statsdListenTCP      = kingpin.Flag("statsd.listen-tcp", "The TCP address on which to receive statsd metric lines. \"\" disables it.").Default(":9125").String()
		statsdListenUnixgram = kingpin.Flag("statsd.listen-unixgram", "The Unixgram socket path to receive statsd metric lines in datagram. \"\" disables it.").Default("").String()
		// not using Int here because flag diplays default in decimal, 0755 will show as 493
		statsdUnixSocketMode = kingpin.Flag("statsd.unixsocket-mode", "The permission mode of the unix socket.").Default("755").String()
		mappingConfig        = kingpin.Flag("statsd.mapping-config", "Metric mapping configuration file name.").String()
		readBuffer           = kingpin.Flag("statsd.read-buffer", "Size (in bytes) of the operating system's transmit read buffer associated with the UDP or Unixgram connection. Please make sure the kernel parameters net.core.rmem_max is set to a value greater than the value specified.").Int()
		cacheSize            = kingpin.Flag("statsd.cache-size", "Maximum size of your metric mapping cache. Relies on least recently used replacement policy if max size is reached.").Default("1000").Int()
		eventQueueSize       = kingpin.Flag("statsd.event-queue-size", "Size of internal queue for processing events").Default("10000").Int()
		eventFlushThreshold  = kingpin.Flag("statsd.event-flush-threshold", "Number of events to hold in queue before flushing").Default("1000").Int()
		eventFlushInterval   = kingpin.Flag("statsd.event-flush-interval", "Number of events to hold in queue before flushing").Default("200ms").Duration()
		dumpFSMPath          = kingpin.Flag("debug.dump-fsm", "The path to dump internal FSM generated for glob matching as Dot file.").Default("").String()
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("statsd_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	if *statsdListenUDP == "" && *statsdListenTCP == "" && *statsdListenUnixgram == "" {
		log.Fatalln("At least one of UDP/TCP/Unixgram listeners must be specified.")
	}

	log.Infoln("Starting StatsD -> Prometheus Exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())
	log.Infof("Accepting StatsD Traffic: UDP %v, TCP %v, Unixgram %v", *statsdListenUDP, *statsdListenTCP, *statsdListenUnixgram)
	log.Infoln("Accepting Prometheus Requests on", *listenAddress)

	go serveHTTP(*listenAddress, *metricsEndpoint)

	events := make(chan Events, *eventQueueSize)
	defer close(events)
	eventQueue := newEventQueue(events, *eventFlushThreshold, *eventFlushInterval)

	if *statsdListenUDP != "" {
		udpListenAddr := udpAddrFromString(*statsdListenUDP)
		uconn, err := net.ListenUDP("udp", udpListenAddr)
		if err != nil {
			log.Fatal(err)
		}

		if *readBuffer != 0 {
			err = uconn.SetReadBuffer(*readBuffer)
			if err != nil {
				log.Fatal("Error setting UDP read buffer:", err)
			}
		}

		ul := &StatsDUDPListener{conn: uconn, eventHandler: eventQueue}
		go ul.Listen()
	}

	if *statsdListenTCP != "" {
		tcpListenAddr := tcpAddrFromString(*statsdListenTCP)
		tconn, err := net.ListenTCP("tcp", tcpListenAddr)
		if err != nil {
			log.Fatal(err)
		}
		defer tconn.Close()

		tl := &StatsDTCPListener{conn: tconn}
		go tl.Listen()
	}

	if *statsdListenUnixgram != "" {
		var err error
		if _, err = os.Stat(*statsdListenUnixgram); !os.IsNotExist(err) {
			log.Fatalf("Unixgram socket \"%s\" already exists", *statsdListenUnixgram)
		}
		uxgconn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{
			Net:  "unixgram",
			Name: *statsdListenUnixgram,
		})
		if err != nil {
			log.Fatal(err)
		}

		defer uxgconn.Close()

		if *readBuffer != 0 {
			err = uxgconn.SetReadBuffer(*readBuffer)
			if err != nil {
				log.Fatal("Error setting Unixgram read buffer:", err)
			}
		}

		ul := &StatsDUnixgramListener{conn: uxgconn}
		go ul.Listen()

		// if it's an abstract unix domain socket, it won't exist on fs
		// so we can't chmod it either
		if _, err := os.Stat(*statsdListenUnixgram); !os.IsNotExist(err) {
			defer os.Remove(*statsdListenUnixgram)

			// convert the string to octet
			perm, err := strconv.ParseInt("0"+string(*statsdUnixSocketMode), 8, 32)
			if err != nil {
				log.Warnf("Bad permission %s: %v, ignoring\n", *statsdUnixSocketMode, err)
			} else {
				err = os.Chmod(*statsdListenUnixgram, os.FileMode(perm))
				if err != nil {
					log.Warnf("Failed to change unixgram socket permission: %v", err)
				}
			}
		}

	}

	if runtime.GOOS == "linux" {
		watchUDPBuffers(0, 0, 0, 0)
	}

	mapper := &mapper.MetricMapper{MappingsCount: mappingsCount}
	if *mappingConfig != "" {
		err := mapper.InitFromFile(*mappingConfig, *cacheSize)
		if err != nil {
			log.Fatal("Error loading config:", err)
		}
		if *dumpFSMPath != "" {
			err := dumpFSM(mapper, *dumpFSMPath)
			if err != nil {
				log.Fatal("Error dumping FSM:", err)
			}
		}
		go watchConfig(*mappingConfig, mapper, *cacheSize)
	} else {
		mapper.InitCache(*cacheSize)
	}
	exporter := NewExporter(mapper)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go exporter.Listen(events)

	<-signals
}
