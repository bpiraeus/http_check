package main

// import needed packages
import (
	"bufio"
	"io"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
	"runtime"
	"log"
	"encoding/json"
	graphite "./graphite"
	"io/ioutil"
	"errors"
)

// version
const (
	version	string = "HTTP Check Version 0.1"
	author string = "Brett Hawn (bpiraeus@gmail.com)"
	modified string = "Jan 16, 2013"
)

// stash all our configuration pieces into a struct
type Config struct {

	configFile 	string
	url        	string
	logfile    	string
	shost      	string
	gval       	string
	tval       	string
	prefix     	string
	ua		string
	port       	int
	debugLevel 	int
	timeout    	int
	interval   	int
	numthread  	int
	ssl        	bool
	help       	bool
	printcert  	bool
	verbose	   	bool
	quiet	   	bool
	tcpsix     	bool
	tcpfour    	bool
	syslog     	bool
	Version    	bool
	expires    	bool
	zabbix     	bool
	collectd   	bool
	multiple	bool
	gph	   	*graphite.Graphite
}

// struct to contain our worker information
type Worker struct {

	id	int
	in	chan *Work
	out	chan *Work
	control chan int
}

// struct to contain information about the work to be done
type Work struct {

	target	string
	url	string
	ssl	bool
	gph	*graphite.Graphite
	port	int
}

// initialize some variable
var config	Config
var logChan	chan string
var logEnd	chan bool

// disposable variables 
var ginterval	time.Duration
var gtimeout	time.Duration
var gretries	int

//
// functions
//

// initialize everything
func init() {

	flag.BoolVar(&config.help, "h", false, "display help/usage")

	flag.StringVar(&config.configFile, "c", "http_check.cfg", "specify config file")
	flag.StringVar(&config.url, "u", "http://www.github.com/", "specify url to be requested")
	flag.StringVar(&config.shost, "H", "", "define a single host to test")
	flag.StringVar(&config.logfile, "f", "", "file to log to (unused at this time)")
	flag.StringVar(&config.gval, "G", "", "enables writing to a graphite server, requires host:port combination")
	flag.StringVar(&config.tval, "T", "", "enables writing to tsdb, requires host:port combination")
	flag.StringVar(&config.prefix, "pr", "", "prepend identifier to graphite write")
	flag.StringVar(&config.ua, "ua", "", "modify user agent")
	
	flag.BoolVar(&config.expires, "e", false, "print ssl cert expiration in seconds")
	flag.BoolVar(&config.printcert, "pc", false, "print cert details")
	flag.BoolVar(&config.verbose, "v", false, "enable verbose output")
	flag.BoolVar(&config.quiet, "q", false, "silence error output")
	flag.BoolVar(&config.tcpfour, "4", true, "use IPV4 only")
	flag.BoolVar(&config.tcpsix, "6", false, "use IPV6 only")
	flag.BoolVar(&config.Version, "V", false, "show version")
	flag.BoolVar(&config.zabbix, "Z", false, "output in zabbix parseable format")
	flag.BoolVar(&config.collectd, "C", false, "output in collectd parseable format")
	flag.BoolVar(&config.multiple, "M", false, "output in all defined formats plus stdout")

	flag.IntVar(&config.port, "p", 80, "port to connect to")
	flag.IntVar(&config.timeout, "t", 10, "Timeout in seconds")
	flag.IntVar(&config.debugLevel, "d", 0, "enable debug and set level of debug output")
	flag.IntVar(&config.interval, "I", 60, "define an interval for injection of time span into collection mechanics")
	flag.IntVar(&config.numthread, "N", 1, "number of threads to use")
	flag.IntVar(&gretries, "gr", 3, "number of times graphite tries to reconnect to server")

	flag.DurationVar(&ginterval, "gi", 1 * time.Second, "Seconds between writes to carbon server")
	flag.DurationVar(&gtimeout, "gt", 1 * time.Second, "timeout value for writes/connects to carbon server in seconds")

	// start our logger as we initialize
	logChan = make(chan string, 1000)
	logEnd = make(chan bool)
}

// main loop
func main() {

	// start our logger
	go logRoutine()

	// prep some channels
	dbg(3, "prepping channels..\n")
	in := make(chan *Work, 1000)
	out := make(chan *Work, 1000)
	control := make(chan int)

	// initialize everything to it's defaults
	flag.Parse()
	dbg(3, "Initialized everything\n")

	dbg(3, "we default to -4, did we enable -6?\n")
	// can't use both protocols simultaneously
	if config.tcpsix {
		dbg(3, "tcpsix enabled, disabling tcpfour\n")
		config.tcpfour = false
	}

	dbg(3, "did we ask for help?\n")
	// if -h is specified, print help and exit
	if config.help {
		doUsage()
	}

	dbg(3, "did we ask for version?\n")
	// if -V is specified, print version info and exit
	if config.Version {
		doVersion()
	}	

	dbg(3, "checking for ssl..\n")
	// parse the URL we've been passed and check the scheme to auto-detect http vs https
	u, uerr := url.Parse(config.url)
	if uerr != nil {
		log.Fatalf("Could not parse %s , please provide a properly formed URL", config.url)
	}
	if u.Scheme == "https" {
		config.ssl = true
		if config.port == 80 {
			config.port = 443;
		}
	}

	dbg(3, "Did we enable graphite writing?\n")
	// if graphite is defined, setup a graphite writer
	if config.gval != "" {
		var gd bool = false
		if config.debugLevel != 0 {
			gd = true
		}
		gph, err := graphite.NewGraphite(config.gval, ginterval, gtimeout, gretries, gd)
		if err != nil {
			dbgError("Could not initialize graphite writer: %s\n", err)
			os.Exit(1)
		}
		config.gph = gph
	}
		
	dbg(3, "Fire up the workers\n")
	// initialize our workers
	for n := 0; n < config.numthread; n++ {
		w := newWorker(n, in, out, control)
		w.dbg(3, "Worker %d started...\n", w.id)
		go w.startWorker()
	}

	// feed the workers or they'll form a union and revolt against our management
	dbg(3, "Ok, time to feed the workers\n")
	go func() {
		dbg(3, "pre-heating the oven\n")
		// if we are running with a single host
		if config.shost != "" {
			dbg(3, "Single host mode for %s\n", config.shost)
			in <- &Work{target: config.shost, port: config.port, url: config.url, ssl: config.ssl, gph: config.gph}
		} else {
			
		// we're in batch mode
			_, serr := os.Stat(config.configFile)
			if serr != nil {
				dbgError("%s not found, did you mean to use -H?", config.configFile)
				os.Exit(1)
			}
			dbg(3, "Batch mode...\n")
			lines := make(chan string, 1000)
			go readLinesInFile(config.configFile, lines)
			for host := range lines {
				dbg(3, "Queuing host: %s\n", host)
				in <- &Work{target: host, port: config.port, url: config.url, ssl: config.ssl, gph: config.gph}
			}
		}
		close(in)
	}()
	dbg(3, "lunch is ready\n")

	// close our output channel when we're done
	go func() {
		for n := 0; n < config.numthread; n++ {
			<-control
		}
		close(out)
	}()

	// process the outbound queue, but we're not actually doing anything here
	for _ = range out {
	}

	// we're done, wrap it up
	dbg(3, "Wrapping it up...\n")

	// IF we have one, close our graphite channel
	if config.gph != nil {
	  config.gph.Shutdown()
	}

	close(logChan)
	<-logEnd

}

// help message
func doUsage() {

	fmt.Fprintf(os.Stdout, "http_check <options>\n\n")
	fmt.Fprintf(os.Stdout, "Options:\n")
	fmt.Fprintf(os.Stdout, "-h - help, this message\n")
	fmt.Fprintf(os.Stdout, "-v - be verbose and output details (default is off)\n")
	fmt.Fprintf(os.Stdout, "-V - Show Version\n")
	fmt.Fprintf(os.Stdout, "-c - config file, default is http_check.cfg\n")
	fmt.Fprintf(os.Stdout, "-H - test a single host\n")
	fmt.Fprintf(os.Stdout, "-p - port, default is 80 (defaults to 443 if -s is enabled)\n")
	fmt.Fprintf(os.Stdout, "-e - print expiration time in days for ssl cert\n")
	fmt.Fprintf(os.Stdout, "-pc - print certificate details if connection is ssl\n")
	fmt.Fprintf(os.Stdout, "-t - timeout value in seconds, default is 10s\n")
	fmt.Fprintf(os.Stdout, "-6 - IPv6 only\n")
	fmt.Fprintf(os.Stdout, "-4 - IPv4 only\n")
	fmt.Fprintf(os.Stdout, "-d - debug level, default is 0 (no debugging), 1 is base debug, 2 is more verbose, 3 is highest verbosity\n")
	fmt.Fprintf(os.Stdout, "-C - collectd parseable format\n")
	fmt.Fprintf(os.Stdout, "-T - write metrics to TSDB (requires tsdbserver:port combination)\n")
	fmt.Fprintf(os.Stdout, "-G - enable writing to Graphite server eg. -G localhost:2003\n")
	fmt.Fprintf(os.Stdout, "-gr - number of times to try and reconnect to the graphite server if the connection is broken\n")
	fmt.Fprintf(os.Stdout, "-gi - interval in seconds between writes to the graphite server\n")
	fmt.Fprintf(os.Stdout, "-gt - timeout in seconds for writes/connctions to graphite server\n")
	fmt.Fprintf(os.Stdout, "-I - specify interval for writing timespan information to collection system\n")
	fmt.Fprintf(os.Stdout, "-Z - zabbix parseable format\n")
	fmt.Fprintf(os.Stdout, "-N - Number of threads to use, default is 1\n")
	fmt.Fprintf(os.Stdout, "\n")
	fmt.Fprintf(os.Stdout, "Config file is a list of hosts to be tested, each host on it's own line\n")
	fmt.Fprintf(os.Stdout, "auto-detects SSL based on url scheme\n")
	fmt.Fprintf(os.Stdout, "If neither -4 or -6 are enumerated, it will default to IPv4\n")
	fmt.Fprintf(os.Stdout, "\n")
	fmt.Fprintf(os.Stdout, "Version: %s\n", version)
	fmt.Fprintf(os.Stdout, "Author: %s\n", author)
	fmt.Fprintf(os.Stdout, "Last Modified: %s\n", modified)
	fmt.Fprintf(os.Stdout, "\n")
	os.Exit(0)
}

// print version info
func doVersion() {

	fmt.Fprintf(os.Stdout, "Version: %s\n", version)
	os.Exit(0)
}

// safe wrapper for testHost so we can recover from gross errors which would otherwise panic() the system and cause an early exit
func (w *Worker) safeTestHost(work *Work) {
	defer func() {
		if err := recover(); err != nil {
			w.dbgError("%s is probably quite broken...\n", work.target)
		}
	}()
	w.testHost(work)
}
	
// function to do the actual testing and output the necessary bits
// we don't really care about ordering here otherwise we'd need to do a better job of organizing the output
func ( w *Worker) testHost(work *Work) {

	// more debug
	w.dbg(3, "Ok.. prepping to test %s\n", work.target)

	// pre-define myConn/tlsConn/client
	var myConn net.Conn
	var tlsConn *tls.Conn
	var client *httputil.ClientConn

	// need a couple of vars for holding ip addresses and default them to being empty
	var ip4 string = ""
	var ip6 string = ""

	// and a var to stash our full host:port combo in
	var address string = ""
	
	// create a buffer we'll use later for reading client body
	buffer := make([]byte, 1024)
	
	// build our request
	request, err := http.NewRequest("GET", work.url, nil)
	if err != nil {
		w.dbgError("Failed to build request for %s : %s\n", work.url, err)
		return
	}
	if config.ua == "" {
		request.Header.Set("User-Agent", version)
	} else {
		request.Header.Set("User-Agent", config.ua)
	}

	// debug
	w.dbg(3, "Request: %+v\n", request)

	// set a timer for the connection
	timestart := time.Now()

	ipaddr, ierr := net.LookupHost(work.target)
	if ierr != nil {
		w.dbgError("Failed to lookup %s : %s", work.target, ierr)
		return
	}

	for _, x := range ipaddr {
		if len(x) > 16 {
			if ip6 == "" {
				ip6 = x
			}
		} else if ip4 == "" {
			ip4 = x
		}
	}

	timestop := time.Now()
	iplookup := (timestop.Sub(timestart))

	// build our address string
	if config.tcpfour {
		address = fmt.Sprintf("%s:%d", ip4, work.port)
	} else {
		address = fmt.Sprintf("[%s]:%d", ip6, work.port)
	}

	// debug
	w.dbg(1, "Attempting to connect to %s\n", address)

	// since we want some very low level access to bits and pieces, we're going to have to use tcp dial vs the native http client
	// create a net.conn
	w.dbg(1, "Connecting to %s\n", address)
	if config.tcpfour {
		myConn, err = net.DialTimeout("tcp4", address, time.Duration(config.timeout) * time.Second)
	} else if config.tcpsix {
		myConn, err = net.DialTimeout("tcp6", address, time.Duration(config.timeout) * time.Second)
	} else {
		myConn, err = net.DialTimeout("tcp", address, time.Duration(config.timeout) * time.Second)
	}
		
	if err != nil {
		w.dbgError("Could not connect to %s : %s\n", address, err)
		return
	}
	w.dbg(2, "Connected to %s\n", address)

	// get a time reading on how long it took to connect to the socket
	timestop = time.Now()
	tcpConnect := (timestop.Sub(timestart))

	// defer close
	defer myConn.Close()

	// need to add some deadlines so we don't sit around indefintely - 5s is more than sufficient
	myConn.SetDeadline(time.Now().Add(time.Duration(5 * time.Second)))

	// if we're an ssl connection, we need a few extra steps here
	if work.ssl {
		w.dbg(1, "Starting SSL procedures...\n")

		// default to allowing insecure ssl
		tlsConfig := tls.Config{InsecureSkipVerify: true}

		// create a real tls connection
		tlsConn = tls.Client(myConn, &tlsConfig)

		// do our SSL negotiation
		err = tlsConn.Handshake()
		if err != nil {
			w.dbgError("Could not negotiate tls handshake on %s : %s\n", address, err)
			return
		}

		// defer closing this connection as well
		defer tlsConn.Close()
	}
	
	// get a time reading on how long it took to negotiate ssl
	timestop = time.Now()
	sslHandshake := (timestop.Sub(timestart))

        // get our state
	if work.ssl {
		state := tlsConn.ConnectionState()
		w.dbg(2, "Handshake Complete: %t\n", state.HandshakeComplete)
		w.dbg(2, "Mutual: %t\n", state.NegotiatedProtocolIsMutual)
	}

	// debug
	w.dbg(3, "Converting to an HTTP client connection...\n")

	// convert to an http connection
	if work.ssl {
		client = httputil.NewProxyClientConn(tlsConn, nil)
	} else {
		client = httputil.NewProxyClientConn(myConn, nil)
	}

	// debug
	w.dbg(1, "Making GET request\n")

	// write our request to the socket
	err = client.Write(request)
	if err != nil {
		w.dbgError("Error writing request : %s\n", err)
		return
	}

	// read our response headers
	response, err := client.Read(request)
	if err != nil {
		// did we get a 400?
		if response.StatusCode == 400 {
			w.dbgError("400 response received.. \n")
			return
		}
		// did we get a 404?
		if response.StatusCode == 404 {
			w.dbgError("404 response received.. \n")
			return
		}
		// any other error, exit out
		w.dbgError("Error reading response : %s\n", err)
		return
	}

	w.dbg(1, "Status: %s\n", response.Status)

	// did we get a response?
	if len(response.Header) == 0 {
		w.dbgError("0 length response, something probably broke")
		return
	}

	// measure response header time
	timestop = time.Now()
	respTime := (timestop.Sub(timestart))

	// defer close since we still want to read the body of the object
	defer response.Body.Close()

	// build a reader
	br := bufio.NewReader(response.Body)

	// now read the first byte
	c, err := br.ReadByte()
	if err != nil {
		w.dbgError("Could not read data: %s\n", err)
		return
	}

	// measure our first byte time, this is normally 0ms however longer periods could be indicative of a problem
	timestop = time.Now()
	byteTime := (timestop.Sub(timestart))

	// ok, read the rest of the response
	n, err := br.Read(buffer)
	count := n
	for err != io.EOF {
		n, err = br.Read(buffer)
		count += n
	}

	// did we fail to read everything?
	if err != nil && err != io.EOF {
		w.dbgError("Error on data read, continuing with only %d bytes of %s read\n", count+1, response.Header.Get("Content-Length"))
	}

	// measure our overall time to proccess the entire transaction
	timestop = time.Now()
	totalTime := (timestop.Sub(timestart))

	w.dbg(2, "Received %d bytes total\n", count+1)
	w.dbg(3, "Response: %s%s\n", string(c), string(buffer))

	// shut down the client
	client.Close()
	
	// properly close out our other connections
	myConn.Close()
	if work.ssl {
		tlsConn.Close()
	}

	if work.gph != nil {
		// Graphite selected
		w.dbg(2, "Graphite selected...\n")
		gname := strings.Replace(work.target, ".", "_", -1)
		prefix := ""
		if config.prefix != "" {
			prefix = fmt.Sprintf("%s/", config.prefix)
		}
		if work.ssl {
			work.gph.PostOne(fmt.Sprintf("%s%s/ssl_port_%d/dns_time", prefix, gname, work.port), float64(iplookup / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/ssl_port_%d/connect_time", prefix, gname, work.port), float64(tcpConnect / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/ssl_port_%d/ssl_time", prefix, gname, work.port), float64(sslHandshake / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/ssl_port_%d/response_time", prefix, gname, work.port), float64(respTime / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/ssl_port_%d/byte_time", prefix, gname, work.port), float64(byteTime / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/ssl_port_%d/total_time", prefix, gname, work.port), float64(totalTime / 1000000))
		} else {
			work.gph.PostOne(fmt.Sprintf("%s%s/http_port_%d/dns_time", prefix, gname, work.port), float64(iplookup / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/http_port_%d/connect_time", prefix, gname, work.port), float64(tcpConnect / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/http_port_%d/response_time", prefix, gname, work.port), float64(respTime / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/http_port_%d/byte_time", prefix, gname, work.port), float64(byteTime / 1000000))
			work.gph.PostOne(fmt.Sprintf("%s%s/http_port_%d/total_time", prefix, gname, work.port), float64(totalTime / 1000000))
		}

		// if we've defined multiple, output in all defined formats + stdout
		if ! config.multiple {
			return
		}
	}

	// PublishMetric(host string, instance string, key string, value int64) (error Error) {

	// add tsdb writing
	if config.tval != "" {
		w.dbg(2, "Writing to tsdb server...\n")
		var METRIC []interface{}
		var tags map[string]interface{}
		var metric map[string]interface{}

		tstamp := int64(time.Now().Unix())

		if work.ssl {
			tags = map[string]interface{}{"host": work.target, "port": work.port, "type": "ssl"}
			metric = map[string]interface{}{"metric": "handshake_time", "value": int64(sslHandshake / 1000000), "timestamp": tstamp, "tags": tags}
			METRIC = append(METRIC, metric);
		} else {
			tags = map[string]interface{}{"Host": work.target, "Port": work.port, "Type": "http"}
		}
		metric = map[string]interface{}{"metric": "dns_lookup", "value": int64(iplookup / 1000000), "timestamp": tstamp, "tags": tags}
		METRIC = append(METRIC, metric);
		metric = map[string]interface{}{"metric": "connect_time", "value": int64(tcpConnect / 1000000), "timestamp": tstamp, "tags": tags}
		METRIC = append(METRIC, metric);
		metric = map[string]interface{}{"metric": "response_time", "value": int64(respTime / 1000000), "timestamp": tstamp, "tags": tags}
		METRIC = append(METRIC, metric);
		metric = map[string]interface{}{"metric": "firstbyte_time", "value": int64(byteTime / 1000000), "timestamp": tstamp, "tags": tags}
		METRIC = append(METRIC, metric);
		metric = map[string]interface{}{"metric": "total_time", "value": int64(totalTime / 1000000), "timestamp": tstamp, "tags": tags}
		METRIC = append(METRIC, metric);

		b, _ := json.Marshal(METRIC)
		err = writeTSDB(b)
		if err != nil {
			dbg(1, "Error writing to tsdb server: %s\n", err)
		}
			
		// if we've defined multiple, output in all defined formats + stdout
		if ! config.multiple {
			return
		}
	}

	// add collectd style output
	if config.collectd {
		if work.ssl {
			fmt.Fprintf(os.Stdout, "PUTVAL %s/ssl_port_%d/milliseconds-dns_time interval=%d N:%d\n", work.target, work.port, config.interval, iplookup / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/ssl_port_%d/milliseconds-connect_time interval=%d N:%d\n", work.target, work.port, config.interval, tcpConnect / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/ssl_port_%d/milliseconds-ssl_time interval=%d N:%d\n", work.target, work.port, config.interval, sslHandshake / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/ssl_port_%d/milliseconds-response_time interval=%d N:%d\n", work.target, work.port, config.interval, respTime / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/ssl_port_%d/milliseconds-byte_time interval=%d N:%d\n", work.target, work.port, config.interval, byteTime / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/ssl_port_%d/milliseconds-total_time interval=%d N:%d\n", work.target, work.port, config.interval, totalTime / 1000000)
		} else {
			fmt.Fprintf(os.Stdout, "PUTVAL %s/http_port_%d/milliseconds-dns_time interval=%d N:%d\n", work.target, work.port, config.interval, iplookup / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/http_port_%d/milliseconds-connect_time interval=%d N:%d\n", work.target, work.port, config.interval, tcpConnect / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/http_port_%d/milliseconds-response_time interval=%d N:%d\n", work.target, work.port, config.interval, respTime / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/http_port_%d/milliseconds-byte_time interval=%d N:%d\n", work.target, work.port, config.interval, byteTime / 1000000)
			fmt.Fprintf(os.Stdout, "PUTVAL %s/http_port_%d/milliseconds-total_time interval=%d N:%d\n", work.target, work.port, config.interval, totalTime / 1000000)
		}

		// if we've defined multiple, output in all defined formats + stdout
		if ! config.multiple {
			return
		}
	}

	// add function for zabbix
	if config.zabbix {
		if work.ssl {
			fmt.Fprintf(os.Stdout, "https_host=%s, port=%d, dns_lookup=%dms, socket_connect=%dms, ssl_negotiation=%dms, response_time=%dms, first_byte=%dms, total_time=%dms ",
				work.target, work.port, iplookup / 1000000, tcpConnect / 1000000, sslHandshake / 1000000, respTime / 1000000, byteTime / 1000000, totalTime / 1000000)
			if config.expires {
				i := 0
				state := tlsConn.ConnectionState()
				for _, v := range state.PeerCertificates {
					if i == 0 {
						myT := time.Now()
						myExpires := v.NotAfter.Sub(myT)
						fmt.Fprintf(os.Stdout, "expires=%dd\n", myExpires / 8.64e13 )
						i++
					}
				}
			} else {
				fmt.Fprintf(os.Stdout, "\n")
			}
		} else {
			fmt.Fprintf(os.Stdout, "host=%s, port=%d, dns_lookup=%dms, socket_connect=%dms, response_time=%dms, first_byte=%dms, total_time=%dms\n",
				work.target, work.port, iplookup / 1000000, tcpConnect / 1000000, respTime / 1000000, byteTime / 1000000, totalTime / 1000000)
		}

		// if we've defined multiple, output in all defined formats + stdout
		if ! config.multiple {
			return
		}
	}

	// print out our values
	if ( ( ! config.multiple ) || ( config.verbose ) ) {
		if work.ssl {
			log.Printf("Host: %s -> DNS Lookup: %dms, Socket Connect: %dms, SSL Negotiation: %dms, Response Time: %dms, 1st Byte: %dms, Total Time: %dms\n", 
				work.target, iplookup / 1000000, tcpConnect / 1000000, sslHandshake / 1000000, respTime / 1000000, byteTime / 1000000, totalTime / 1000000)
		} else {
			log.Printf("Host: %s -> DNS Lookup: %dms, Socket Connect: %dms, Response Time: %dms, 1st Byte: %dms, Total Time: %dms\n", 
				work.target, iplookup / 1000000, tcpConnect / 1000000, respTime / 1000000, byteTime / 1000000, totalTime / 1000000)
		}

		if work.ssl {
			if config.expires == true {
				i := 0
				state := tlsConn.ConnectionState()
				for _, v := range state.PeerCertificates {
					if i == 0 {
						myT := time.Now()
						myExpires := v.NotAfter.Sub(myT)
						fmt.Fprintf(os.Stdout, "Cert expires in: %d days\n", myExpires / 8.64e13 )
						i++
					}
				}
			}
		}
	}

	if config.verbose {

		log.Printf("StatusCode: %d\n", response.StatusCode)
		log.Printf("ProtoCol: %s\n", response.Proto)
		for k,v := range response.Header {
			log.Printf("%s: %v\n", k, v)
		}
		if work.ssl {
			if config.printcert == true {
				i := 0
				state := tlsConn.ConnectionState() 
				for _, v := range state.PeerCertificates {
					 if i == 0 { 
						sslFrom := v.NotBefore 
						sslTo := v.NotAfter 
						log.Printf("Server key information:") 
						log.Printf("\tCN:\t%v\n\tOU:\t%v\n\tOrg:\t%v\n", v.Subject.CommonName, v.Subject.OrganizationalUnit, v.Subject.Organization)
						log.Printf("\tCity:\t%v\n\tState:\t%v\n\tCountry:%v\n", v.Subject.Locality, v.Subject.Province, v.Subject.Country)
						log.Printf("SSL Certificate Valid:\n\tFrom: %v\n\tTo: %v\n", sslFrom, sslTo)
						log.Printf("Valid Certificate DNS:\n")
						if len(v.DNSNames) >= 1 { 
							for dns := range v.DNSNames {
								log.Printf("\t%v\n", v.DNSNames[dns])
							}
						} else {
							log.Printf("\t%v\n", v.Subject.CommonName)
						}
						i++
					} else if i == 1 {
						log.Printf("Issued by:\n\t%v\n\t%v\n\t%v\n", v.Subject.CommonName, v.Subject.OrganizationalUnit, v.Subject.Organization)
						i++
					} else {
						// we're done here, lets move on
						break
					}
				}
			}
		}

		// throw in a new line to pretty it up
		log.Printf("")
	}

	return

}

// organize our output
func logRoutine() {
	
	for s := range logChan {
		os.Stdout.WriteString(s)
	}
	logEnd <- true
}

// standardize error mechanics
func dbgError(errfmt string, a ...interface{}) {

	if ! config.quiet {
		s := fmt.Sprintf(errfmt+"\n", a...)
		logChan <- s
	}
}

// due to threading we need to handle this differently
func (w *Worker) dbgError(errfmt string, a ...interface{}) {

	errfmt = fmt.Sprintf("Worker %d %s", w.id, errfmt)
	dbgError( errfmt, a...)
}

// standardize debug mechanics
func dbg(level int, errfmt string, a ...interface{}) {

	if level <= config.debugLevel {
		s := fmt.Sprintf(errfmt+"\n", a...)
		logChan <- s
	}
}

// due to threading we need to handle this differently
func (w *Worker) dbg(d int, errfmt string, a ...interface{}) {

	errfmt = fmt.Sprintf("Worker %d %s", w.id, errfmt)
	dbg(d, errfmt, a...)
}

func ( w *Worker) startWorker() {

	w.dbg(1, "%d starting work routine\n", w.id)
	
	// pull in information for the workers
	for work := range w.in {
		w.dbg(3, "I got %s as a target\n", work.target)
		w.safeTestHost(work)
		w.out <- work
	}

	w.dbg(1, "ending work..\n")
	w.control <- w.id
}

// liberally snarfed from previous work
func readLinesInFile(fname string, hosts chan string) {

	// debug! more debug!
	dbg(3, "Attempting to open %s\n", fname)
	f, err := os.Open(fname)
	if err != nil {
		dbgError("Failed to open %s : %s\n", fname, err)
		return
	}
	// defer close
	defer f.Close()

	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	for err == nil {
		line = strings.TrimSpace(line)
		dbg(3, "read %s\n", line)
		hosts <- line
		line, err = r.ReadString('\n')
	}
	close(hosts)

	return
}

// initialize the worker
func newWorker(id int, in, out chan *Work, control chan int) *Worker {

	dbg(3, "Received %d for worker ID\n", id)
	return &Worker{id: id, in: in, out: out, control: control}
}

func writeTSDB (metric []byte) (err error) {

	tserver := fmt.Sprintf("http://%s/put/", config.tval)
	// now post out metrics
	response, err := http.Post(tserver, "application/json", strings.NewReader(string(metric)))
	if err != nil {
		return err
	}

	// defer our close on the body of the response
	defer response.Body.Close()

	// read our response body
	_, err = ioutil.ReadAll(response.Body)
	if err != nil {
		return err;
	}

	// check our return code
	if response.StatusCode != 200 {
		err := errors.New(fmt.Sprintf("Post returned %d status code!", response.StatusCode))
		return err;
	}
	
	return nil
}
