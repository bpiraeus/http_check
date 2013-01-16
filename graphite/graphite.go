package graphite

// import needed packages
import (

	"fmt"
	"time"
	"net"
	"log"
)

// This defined the Graphite structure which contains the necessary bits for
// a functioning object. 
type Graphite struct {
	
	endpoint	string			// host:port pair for the Carbon server
	interval	time.Duration		// how often do we write to the Carbon server
	timeout		time.Duration		// how long will we wait to connect/write
	retries		int			// how many times will we retry
	connection	net.Conn		// our net.Conn
	vars		map[string]interface{}	// key:value pairs in an accessible interface
	registers	chan namedvars		// channel for registering vars
	shutdown	chan chan bool		// control channel for closing out or Graphite structure cleanly
	debug		bool			// enable debug
}

// a sub struct for key value pairs
type namedvars struct {
	
	name	string
	val	interface{}	// Allow for anonymous data to be placed into the pair rather than hard code types
}

// Function to build and return a Graphite object
func NewGraphite(endpoint string, interval, timeout time.Duration, retries int, debug bool) (*Graphite, error) {

	// debug
	if debug {
		log.Printf("Generating a new Graphite structure")
	}

	// formalize our Graphite structure
	g := &Graphite {
		endpoint:	endpoint,
		interval:	interval,
		timeout:	timeout,
		connection:	nil,
		vars:		make(map[string]interface{}),
		registers:	make(chan namedvars),
		shutdown:	make(chan chan bool),
		debug:		debug,
	}
	
	// connect to our Carbon server or return a failed Graphite structure
	err := g.reconnect()
	if err != nil {
		if g.debug {
			log.Printf("Failed to connect to Carbon server: %s", err)
		}
		return nil, err
	}

	// start our service loop
	go g.loop()

	// return the Graphite structure and nil error
	return g, nil
}

// Function to insert key:value pairs into the channel
func (g *Graphite) Insert(name string, val interface{}) {

	if g.debug {
		log.Printf("Inserting %s:%g pair\n", name, val)
	}
	
	// insert the pair
	g.registers <- namedvars{name: name, val: val}

}

// Function to shutdown the Graphite object cleanly
func (g *Graphite) Shutdown() {
	
	if g.debug {
		log.Printf("Shutdown called, notifying Graphite loop to close")
	}
	q := make(chan bool)
	g.shutdown <-q
	<-q

}

// Loop on our channels every interval to: create our mapped pair, write any pending data, or shutdown our object
// primary publishing mechanic, loop through at every ~ every ticker interval and register, publish, or close  out
func (g *Graphite) loop() {

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
			case nv := <- g.registers:
				g.vars[nv.name] = nv.val
			case <- ticker.C:
				g.PostAll()
			case q := <- g.shutdown:
				g.connection.Close()
				g.connection = nil
				q <- true
				return
		}
	}
}

// Wrapper function for our writer that cycles through any available data pairs and then sends them to the writer
func (g *Graphite) PostAll() {

	for name, v := range g.vars {
		err := g.PostOne(name, v)
		if err != nil {
			log.Printf("Graphite: %s: %s", name, err)
		}
	}
}

// write to our carbon server, if the connection is broken, we will retry for the number of retries specified before failing out entirely
func (g *Graphite) PostOne(name string, val interface{}) (error) {

	if g.connection == nil {
		if g.debug {
			log.Printf("Lost our connection to the Carbon server, attempting to reconnect..")
		}
		for i := 0; i < g.retries; i++ {
			err := g.reconnect()
			if err != nil {
				log.Printf("Failed reconnect #%d: %s", i, err)
			}
		}
	}

	// if we're in debug mode, write our key:value pair to stdout and return
	if g.debug {
		log.Printf("%s -> %g", name, val)
		return nil
	}

	// otherwise, build a timeout so we don't stall trying to write to the Carbon server
	deadline := time.Now().Add(g.timeout)
	err := g.connection.SetWriteDeadline(deadline)
	if err != nil {
		g.connection = nil
		return fmt.Errorf("Failed to set write deadline: %s", err)
	}

	// build a []byte for our data packet
	b := []byte(fmt.Sprintf("%s %v %d\n", name, val, time.Now().Unix()))

	// write our data out to the Carbon server, error if something breaks during the write
	n, err := g.connection.Write(b)
	if err != nil {
		g.connection = nil
		return fmt.Errorf("Failed to write to carbon server: %s", err)
	}

	// if we get a short write, we should notify
	if n != len(b) {
		g.connection = nil
		return fmt.Errorf("%s = %v : Short write %d bytes written of %d: %s", name, val, n, len(b), err)
	}

	// if everything went smoothly, return a nil error
	return nil
}

// Function to establish our net.Conn to the Carbon server
func (g *Graphite) reconnect() (error) {

	if g.debug {
		log.Printf("Connecting to Carbon server at %s", g.endpoint)
	}
	conn, err := net.DialTimeout("tcp", g.endpoint, g.timeout)
	if err != nil {
		return err
	}
	g.connection = conn
	return nil
}
