package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// URI to ping
var uri = ""

//var httpPort = flag.Int("port", -1, "port to connect to the http server")
//var hostName = flag.String("host", "wikipedia.org", "host name to ask DNS server to resolve")
//var recordType = flag.String("rdatatype", "A", "DNS record type of the query")
var postBody = flag.String("d", "", "the body of a POST or PUT request; from file use @filename")
var httpMethod = flag.String("X", "GET", "HTTP method to use")
var count = flag.Int("c", 10, "number of times to query")
var interval = flag.Duration("W", time.Second*1, "wait time between pings")
var timeout = flag.Duration("t", time.Second*2, "amount of time to wait for a response")

// atomic -- 0 if running, non-zero if exiting
var stopping int32

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			`Usage:
  %s [options] [uri]

Measure response time to the given web server by asking it to respond to an HTTP GET. 

OPTIONS
  -h                    show this help
  -c <int>              number of times to query (default 10)
  -W <duration>         wait time between pings (default 1s)
  -t <duration>         amount of time to wait for a server response (default 2s)
`, os.Args[0])

	}
	flag.Parse()

	if len(flag.Args()) != 1 {
		flag.Usage()
		os.Exit(2)
	}

	uri = flag.Args()[0]

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for range signalChan {
			if isStopping() {
				// second ctrl-C means immediate exit
				os.Exit(0)
			}
			atomic.StoreInt32(&stopping, 1)
			cancel()
		}
	}()

	// make sure our URI is valid
	url := parseURL(uri)
	if url == nil {
		fmt.Fprintf(os.Stderr, "Error: invalid URI %v", uri)
		os.Exit(1)
	}
	host := url.Hostname()
	port := 80
	path := url.Path
	if p := url.Port(); p != "" {
		// parse the port
		var err error
		port, err = strconv.Atoi(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid port: %v", p)
			os.Exit(1)
		}
	} else if url.Scheme == "https" {
		port = 443
	}

	// make sure our web server is a legit IP
	if ip := net.ParseIP(host); ip == nil {
		// not an IP, so resolve it as a DNS name
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			fmt.Fprintf(os.Stderr, "Error: cannot resolve server hostname: %v\n", host)
			os.Exit(1)
		}

		host = ips[0].String()
	}

	if path == "" {
		path = "/"
	}

	fmt.Printf("PING %s: %s:%v (%s), %s \n", strings.ToUpper(url.Scheme), url.Hostname(), port, path, *httpMethod)

	var responseTimes []time.Duration
	var requests int

	client := &http.Client{
		Timeout: *timeout,
	}

	req, err := http.NewRequest(*httpMethod, url.String(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v", err)
		os.Exit(3)
	}
	req = req.WithContext(ctx)

	for i := 0; i < *count; i++ {
		if isStopping() {
			break
		}

		requests++
		// TODO: https call
		start := time.Now()
		resp, err := client.Do(req)
		dur := time.Now().Sub(start)
		//fmt.Printf("Response: %#v", resp)
		if err != nil {
			if e, ok := err.(*net.OpError); ok {
				if e.Timeout() {
					fmt.Printf("Request timeout for seq %v\n", i)
					continue
				}
			}
			// all other errors are considered fatal for now
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		//len := resp.ContentLength

		// discard the body
		len, _ := io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()

		responseTimes = append(responseTimes, dur)
		fmt.Printf("%d bytes from %s: seq=%-3d time=%-12v %v\n", len, host, i, fmt.Sprintf("%0.3f ms", inMilli(dur)), resp.Status)
		// sleep as needed
		if sleepTime := *interval - dur; sleepTime > 0 {
			time.Sleep(sleepTime)
		}
	}

	lostPercent := 0.0
	if requests > 0 {
		lostPercent = float64(100*(requests-len(responseTimes))) / float64(requests)
	}

	fmt.Printf("\n--- %s httpping statistics ---\n", url.Hostname())
	fmt.Printf("%d requests transmitted, %d responses received, %.1f%% lost\n", requests, len(responseTimes), lostPercent)
	fmt.Printf("round-trip min/avg/max/stddev = %.3f/%.3f/%.3f/%.3f ms\n", min(responseTimes), avg(responseTimes), max(responseTimes), stddev(responseTimes))
}

func parseURL(uri string) *url.URL {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	url, err := url.Parse(uri)
	if err != nil {
		log.Fatalf("could not parse url %q: %v", uri, err)
	}

	if url.Scheme == "" {
		url.Scheme = "http"
		if !strings.HasSuffix(url.Host, ":80") {
			url.Scheme += "s"
		}
	}
	return url
}

func min(times []time.Duration) float64 {
	if len(times) == 0 {
		return 0
	}
	low := times[0]
	for _, t := range times {
		if t < low {
			low = t
		}
	}

	return inMilli(low)
}

func max(times []time.Duration) float64 {
	if len(times) == 0 {
		return 0
	}
	max := times[0]
	for _, t := range times {
		if t > max {
			max = t
		}
	}

	return inMilli(max)
}

func avg(times []time.Duration) float64 {
	if len(times) == 0 {
		return 0
	}
	sum := 0.0
	for _, t := range times {
		sum += inMilli(t)
	}

	return sum / float64(len(times))
}

func stddev(times []time.Duration) float64 {
	if len(times) == 0 {
		return 0
	}

	avg := avg(times)
	sum := 0.0
	for _, t := range times {
		sum += math.Pow(inMilli(t)-avg, 2)
	}

	variance := sum / float64(len(times))

	return math.Sqrt(variance)
}

func inMilli(t time.Duration) float64 {
	return float64(t.Nanoseconds()) / 1000000.0
}

func isStopping() bool {
	return atomic.LoadInt32(&stopping) != 0
}
