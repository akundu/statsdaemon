package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var LogInfo *log.Logger
var LogError *log.Logger

const (
	MAX_UNPROCESSED_PACKETS = 100000
	MAX_UDP_PACKET_SIZE     = 512
)

var signalchan chan os.Signal

type Packet struct {
	Bucket   string
	Value    interface{}
	Modifier string
	Sampling float32
}

type DiffData struct {
	Last_value  float64
	Last_update time.Time
	Last_diff   float64
}

type GaugeData struct {
	Relative bool
	Negative bool
	Value    float64
}

type Float64Slice []float64

func (s Float64Slice) Len() int           { return len(s) }
func (s Float64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s Float64Slice) Less(i, j int) bool { return s[i] < s[j] }

type Percentiles []*Percentile
type Percentile struct {
	float float64
	str   string
}

func (a *Percentiles) Set(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*a = append(*a, &Percentile{f, strings.Replace(s, ".", "_", -1)})
	return nil
}
func (p *Percentile) String() string {
	return p.str
}
func (a *Percentiles) String() string {
	return fmt.Sprintf("%v", *a)
}

var (
	In              = make(chan *Packet, MAX_UNPROCESSED_PACKETS)
	counters        = make(map[string]int64)
	gauges          = make(map[string]float64)
	trackedGauges   = make(map[string]float64)
	timers          = make(map[string]Float64Slice)
	countInactivity = make(map[string]int64)
	sets            = make(map[string][]string)

	//PacketIn        = make(chan []byte, MAX_UNPROCESSED_PACKETS) //the channel that allows the ability to immediately download data from UDP and feed it out to be parsed instead of parsing and then feeding to go handlers to process
	PacketIn     = make(chan string, MAX_UNPROCESSED_PACKETS) //the channel that allows the ability to immediately download data from UDP and feed it out to be parsed instead of parsing and then feeding to go handlers to process
	trackedDiffs = make(map[string]float64)                   //track diffs in value to previous values - so that diffs are tracked and not the whole value
)

func packetHandler(s *Packet) {
	if *receiveCounter != "" {
		v, ok := counters[*receiveCounter]
		if !ok || v < 0 {
			counters[*receiveCounter] = 0
		}
		counters[*receiveCounter] += 1
	}

	switch s.Modifier {
	case "ms":
		_, ok := timers[s.Bucket]
		if !ok {
			var t Float64Slice
			timers[s.Bucket] = t
		}
		timers[s.Bucket] = append(timers[s.Bucket], s.Value.(float64))
	case "g":
		gaugeValue, _ := gauges[s.Bucket]

		gaugeData := s.Value.(GaugeData)
		if gaugeData.Relative {
			if gaugeData.Negative {
				// subtract checking for -ve numbers
				if gaugeData.Value > gaugeValue {
					gaugeValue = 0
				} else {
					gaugeValue -= gaugeData.Value
				}
			} else {
				// watch out for overflows
				if gaugeData.Value > (math.MaxUint64 - gaugeValue) {
					gaugeValue = math.MaxUint64
				} else {
					gaugeValue += gaugeData.Value
				}
			}
		} else {
			gaugeValue = gaugeData.Value
		}

		gauges[s.Bucket] = gaugeValue
	case "c":
		_, ok := counters[s.Bucket]
		if !ok {
			counters[s.Bucket] = 0
		}
		counters[s.Bucket] += int64(float64(s.Value.(int64)) * float64(1/s.Sampling))
	case "s":
		_, ok := sets[s.Bucket]
		if !ok {
			sets[s.Bucket] = make([]string, 0)
		}
		sets[s.Bucket] = append(sets[s.Bucket], s.Value.(string))
	}
}

func submit(deadline time.Time) error {
	var buffer bytes.Buffer
	var num int64

	now := time.Now().Unix()

	if *graphiteAddress == "-" {
		return nil
	}

	client, err := net.Dial("tcp", *graphiteAddress)
	if err != nil {
		if *debug {
			LogInfo.Printf("WARNING: resetting counters when in debug mode")
			processCounters(&buffer, now)
			processGauges(&buffer, now)
			processTimers(&buffer, now, percentThreshold)
			//processDiffs(&buffer, now)
			processSets(&buffer, now)
		}
		errmsg := fmt.Sprintf("dialing %s failed - %s", *graphiteAddress, err)
		return errors.New(errmsg)
	}
	defer client.Close()

	err = client.SetDeadline(deadline)
	if err != nil {
		errmsg := fmt.Sprintf("could not set deadline:", err)
		return errors.New(errmsg)
	}

	num += processCounters(&buffer, now)
	num += processGauges(&buffer, now)
	num += processTimers(&buffer, now, percentThreshold)
	num += processSets(&buffer, now)
	if num == 0 {
		return nil
	}

	if *debug {
		for _, line := range bytes.Split(buffer.Bytes(), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			LogInfo.Printf("DEBUG: %s", line)
		}
	}

	_, err = client.Write(buffer.Bytes())
	if err != nil {
		errmsg := fmt.Sprintf("failed to write stats - %s", err)
		return errors.New(errmsg)
	}

	//LogInfo.Printf("sent %d stats to %s", num, *graphiteAddress)

	return nil
}

func processCounters(buffer *bytes.Buffer, now int64) int64 {
	var num int64
	// continue sending zeros for counters for a short period of time even if we have no new data
	for bucket, value := range counters {
		fmt.Fprintf(buffer, "%s %d %d\n", bucket, value, now)
		delete(counters, bucket)
		countInactivity[bucket] = 0
		num++
	}
	for bucket, purgeCount := range countInactivity {
		if purgeCount > 0 {
			fmt.Fprintf(buffer, "%s %d %d\n", bucket, 0, now)
			num++
		}
		countInactivity[bucket] += 1
		if countInactivity[bucket] > *persistCountKeys {
			delete(countInactivity, bucket)
		}
	}
	return num
}

/*
func processGauges(buffer *bytes.Buffer, now int64) int64 {
    var num int64

    for g, c := range trackedDiffs {
        fmt.Fprintf(buffer, "%s %f %d\n", g, c, now)
        num++
        delete(gauges, g)
    }
    return num
}
*/

func processGauges(buffer *bytes.Buffer, now int64) int64 {
	var num int64

	for g, c := range gauges {
		fmt.Fprintf(buffer, "%s %f %d\n", g, c, now)
		num++
		delete(gauges, g)
	}
	return num
}

func processSets(buffer *bytes.Buffer, now int64) int64 {
	num := int64(len(sets))
	for bucket, set := range sets {

		uniqueSet := map[string]bool{}
		for _, str := range set {
			uniqueSet[str] = true
		}

		fmt.Fprintf(buffer, "%s %d %d\n", bucket, len(uniqueSet), now)
		delete(sets, bucket)
	}
	return num
}

func processTimers(buffer *bytes.Buffer, now int64, pctls Percentiles) int64 {
	var num int64
	for u, t := range timers {
		num++

		sort.Sort(t)
		min := t[0]
		max := t[len(t)-1]
		median := t[uint64(len(t)/2)]
		maxAtThreshold := max
		count := len(t)

		sum := float64(0)
		for _, value := range t {
			sum += value
		}
		mean := float64(sum) / float64(len(t))

		for _, pct := range pctls {
			if len(t) > 1 {
				var abs float64
				if pct.float >= 0 {
					abs = pct.float
				} else {
					abs = 100 + pct.float
				}
				// poor man's math.Round(x):
				// math.Floor(x + 0.5)
				indexOfPerc := int(math.Floor(((abs / 100.0) * float64(count)) + 0.5))
				if pct.float >= 0 {
					indexOfPerc -= 1 // index offset=0
				}
				maxAtThreshold = t[indexOfPerc]
			}

			var tmpl string
			var pctstr string
			if pct.float >= 0 {
				tmpl = "%s.upper_%s %0.2f %d\n"
				pctstr = pct.str
			} else {
				tmpl = "%s.lower_%s %0.2f %d\n"
				pctstr = pct.str[1:]
			}
			fmt.Fprintf(buffer, tmpl, u, pctstr, maxAtThreshold, now)
		}

		fmt.Fprintf(buffer, "%s.mean %0.2f %d\n", u, mean, now)
		fmt.Fprintf(buffer, "%s.median %0.2f %d\n", u, median, now)
		fmt.Fprintf(buffer, "%s.upper %0.2f %d\n", u, max, now)
		fmt.Fprintf(buffer, "%s.lower %0.2f %d\n", u, min, now)
		fmt.Fprintf(buffer, "%s.count %d %d\n", u, count, now)

		delete(timers, u)
	}
	return num
}

func parseMessageString(data string) []*Packet {
	return parseMessage([]byte(data))
}

func parseMessage(data []byte) []*Packet {
	var (
		output []*Packet
		input  []byte
	)

	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		input = line

		index := bytes.IndexByte(input, ':')
		if index < 0 || index == len(input)-1 {
			if *debug {
				LogError.Printf("ERROR: failed to parse line: %s\n", string(line))
			}
			continue
		}

		name := input[:index]

		index++
		input = input[index:]

		index = bytes.IndexByte(input, '|')
		if index < 0 || index == len(input)-1 {
			if *debug {
				LogError.Printf("ERROR: failed to parse line: %s\n", string(line))
			}
			continue
		}

		val := input[:index]
		index++

		prefixToAdd := *prefix
		var mtypeStr string

		if input[index] == 'm' {
			index++
			if index >= len(input) || input[index] != 's' {
				if *debug {
					LogError.Printf("ERROR: failed to parse line: %s\n", string(line))
				}
				continue
			}
			mtypeStr = "ms"
			prefixToAdd = *prefix + *prefixTimers
		} else {
			mtypeStr = string(input[index])
		}

		index++
		input = input[index:]

		var (
			value interface{}
			err   error
		)

		if mtypeStr[0] == 'c' {
			value, err = strconv.ParseInt(string(val), 10, 64)
			if err != nil {
				LogError.Printf("ERROR: failed to ParseInt %s - %s on %s", string(val), err, line)
				continue
			}
		} else if mtypeStr[0] == 'g' {
			var relative, negative bool
			var stringToParse string

			switch val[0] {
			case '+', '-':
				relative = true
				negative = val[0] == '-'
				stringToParse = string(val[1:])
			default:
				relative = false
				negative = false
				stringToParse = string(val)
			}

			gaugeValue, err := strconv.ParseFloat(stringToParse, 64)
			if err != nil {
				LogError.Printf("ERROR: failed to ParseFloat %s - %s", string(val), err)
				continue
			}

			value = GaugeData{relative, negative, gaugeValue}
			prefixToAdd = *prefix + *prefixGauges
		} else if mtypeStr[0] == 's' {
			value = string(val)
		} else {
			value, err = strconv.ParseFloat(string(val), 64)
			if err != nil {
				LogError.Printf("ERROR: failed to ParseFloat %s - %s on %s", string(val), err, line)
				continue
			}
		}

		var sampleRate float32 = 1

		if len(input) > 0 && bytes.HasPrefix(input, []byte("|@")) {
			input = input[2:]
			rate, err := strconv.ParseFloat(string(input), 32)
			if err == nil {
				sampleRate = float32(rate)
			}
		}

		packet := &Packet{
			//Bucket:   *prefix + string(name),
			Bucket:   prefixToAdd + string(name),
			Value:    value,
			Modifier: mtypeStr,
			Sampling: sampleRate,
		}
		output = append(output, packet)
	}
	return output
}

func processPacketSentIn() {
	var wg sync.WaitGroup //setup a way to wait for the routines to complete that are listening to the UDP connection
	for num_udp_runners := 0; num_udp_runners < *num_procs_to_run; num_udp_runners++ {
		wg.Add(1) //track the routines
		go func() {
			for {
				s := <-PacketIn
				for _, p := range parseMessageString(s) {
					In <- p
				}
			}
			wg.Done()
		}()
	}

	//wait for the routines to complete
	wg.Wait()
}

var RESPONSE = []byte("Hello")
var DOT = []byte(".")

func httpHandler(w http.ResponseWriter, r *http.Request) {
	path_string := r.URL.Path
	//sample url = "/a/b/c/d/time/stat_name/200" - output stat will be a.b.c.d.stat_name = 200 or a.b.c.d.stat_name|ms 200

	var error_string string
	var error_code int
	path_elements := strings.Split(path_string, "/")
	if len(path_elements) < 4 {
		error_string = "Invalid number of splits provided in URL to stat"
		error_code = http.StatusBadRequest
		http.Error(w, error_string, error_code)
		return
	}

	//find out type of request (count|time|gauge)
	stat_type := string(path_elements[len(path_elements)-3])
	//stat_amount
	stat_amount_string := string(path_elements[len(path_elements)-1])

	//build the stat string
	var stat_bytes []byte
	for i := 1; i < len(path_elements)-3; i++ {
		if i > 1 {
			stat_bytes = append(stat_bytes, DOT...)
		}
		stat_bytes = append(stat_bytes, []byte(path_elements[i])...)
	}
	if len(stat_bytes) > 0 {
		stat_bytes = append(stat_bytes, DOT...)
	}
	stat_bytes = append(stat_bytes, []byte(path_elements[len(path_elements)-2])...)

	var statsd_string string
	switch stat_type {
	case "count":
		statsd_string = fmt.Sprintf("%s:%s|c", string(stat_bytes), stat_amount_string)
	case "time":
		statsd_string = fmt.Sprintf("%s:%s|ms", string(stat_bytes), stat_amount_string)
	case "gauge":
		statsd_string = fmt.Sprintf("%s:%s|g", string(stat_bytes), stat_amount_string)
	default:
		error_code = http.StatusBadRequest
		http.Error(w, "Invalid stat_type format", error_code)
		return
	}

	//track the stat
	PacketIn <- statsd_string

	fmt.Fprintf(w, "%s", fmt.Sprintf("OK: %s", statsd_string))
	return
}

func httpListener() {
	interface_port := *serviceAddress //setup the TCP address to be the same as the UDP port for now
	log.Printf("listening on HTTP %s", interface_port)
	http.HandleFunc("/", httpHandler)
	http.ListenAndServe(interface_port, nil)
}

func udpListener() {
	address, _ := net.ResolveUDPAddr("udp", *serviceAddress)
	log.Printf("listening on UDP %s", address)
	listener, err := net.ListenUDP("udp", address)
	if err != nil {
		log.Fatalf("ERROR: ListenUDP - %s", err)
	}
	defer listener.Close()

	var wg sync.WaitGroup //setup a way to wait for the routines to complete that are listening to the UDP connection
	for num_udp_runners := 0; num_udp_runners < *num_procs_to_run; num_udp_runners++ {
		wg.Add(1) //track the routines

		go func() {
			message := make([]byte, MAX_UDP_PACKET_SIZE)
			for {
				n, remaddr, err := listener.ReadFromUDP(message)
				if err != nil {
					log.Printf("ERROR: reading UDP packet from %+v - %s", remaddr, err)
					continue
				}

				PacketIn <- string(message[:n])
			}
			wg.Done()
		}()
	}
	//wait for the routines to complete
	wg.Wait()
}

func monitor() {
	period := time.Duration(*flushInterval) * time.Second
	ticker := time.NewTicker(period)
	for {
		select {
		case sig := <-signalchan:
			fmt.Printf("!! Caught signal %d... shutting down\n", sig)
			if err := submit(time.Now().Add(period)); err != nil {
				log.Printf("ERROR: %s", err)
			}
			return
		case <-ticker.C:
			if err := submit(time.Now().Add(period)); err != nil {
				log.Printf("ERROR: %s", err)
			}
		case s := <-In: //TODO: ideally this should be handled by multiple threads - will change soon
			packetHandler(s)
		}
	}
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("statsdaemon v%s (built w/%s)\n", VERSION, runtime.Version())
		return
	}
	runtime.GOMAXPROCS(*num_procs_to_run)

	signalchan = make(chan os.Signal, 1)
	signal.Notify(signalchan, syscall.SIGTERM)

	go udpListener()
	go httpListener()
	go processPacketSentIn()
	monitor()
}

var (
	serviceAddress   = flag.String("address", ":8125", "UDP service address")
	graphiteAddress  = flag.String("graphite", "127.0.0.1:2003", "Graphite service address (or - to disable)")
	flushInterval    = flag.Int64("flush-interval", 10, "Flush interval (seconds)")
	debug            = flag.Bool("debug", false, "print statistics sent to graphite")
	showVersion      = flag.Bool("version", false, "print version string")
	persistCountKeys = flag.Int64("persist-count-keys", 60, "number of flush-intervals to persist count keys")
	receiveCounter   = flag.String("receive-counter", "", "Metric name for total metrics received per interval")
	percentThreshold = Percentiles{}
	num_procs_to_run = flag.Int("numCPU", runtime.NumCPU()-1, "num cpus to run on")
	prefix           = flag.String("prefix", "stats.", "Prefix for all stats")
	prefixTimers     = flag.String("prefixTimers", "timers.", "Prefix for all timer stats")
	prefixGauges     = flag.String("prefixGauges", "gauges.", "Prefix for all gauges stats")
)

func init() {
	LogInfo = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	LogError = log.New(os.Stderr, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)

	flag.Var(&percentThreshold, "percent-threshold", "percentile calculation for timers (0-100, may be given multiple times)")
}
