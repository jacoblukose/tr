package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"github.com/buger/goreplay/proto"
	_ "github.com/go-sql-driver/mysql"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

const initialDynamicWorkers = 10

var (
	db_mode int = 1
	db      *sql.DB
)

type response struct {
	payload       []byte
	uuid          []byte
	roundTripTime int64
	startedAt     int64
}

// HTTPOutputConfig struct for holding http output configuration
type HTTPOutputConfig struct {
	redirectLimit int

	stats   bool
	workers int

	elasticSearch string

	Timeout      time.Duration
	OriginalHost bool
	BufferSize   int

	Debug bool

	TrackResponses bool
}

// HTTPOutput plugin manage pool of workers which send request to replayed server
// By default workers pool is dynamic and starts with 10 workers
// You can specify fixed number of workers using `--output-http-workers`
type HTTPOutput struct {
	// Keep this as first element of struct because it guarantees 64bit
	// alignment. atomic.* functions crash on 32bit machines if operand is not
	// aligned at 64bit. See https://github.com/golang/go/issues/599
	activeWorkers int64

	address      string
	limit        int
	queue        chan []byte
	output_queue chan []string
	status       chan bool

	responses chan response

	needWorker chan int

	config *HTTPOutputConfig

	queueStats *GorStat

	elasticSearch *ESPlugin
}

// NewHTTPOutput constructor for HTTPOutput
// Initialize workers
func NewHTTPOutput(address string, config *HTTPOutputConfig) io.Writer {
	o := new(HTTPOutput)

	o.address = address
	o.config = config

	if o.config.stats {
		o.queueStats = NewGorStat("output_http")
	}

	o.queue = make(chan []byte, 1000)
	o.responses = make(chan response, 1000)
	o.needWorker = make(chan int, 1)
	o.output_queue = make(chan []string, 2)
	o.status = make(chan bool)

	// Initial workers count
	if o.config.workers == 0 {
		o.needWorker <- initialDynamicWorkers
	} else {
		o.needWorker <- o.config.workers
	}

	if o.config.elasticSearch != "" {
		o.elasticSearch = new(ESPlugin)
		o.elasticSearch.Init(o.config.elasticSearch)
	}

	go o.workerMaster()

	return o
}

func (o *HTTPOutput) workerMaster() {
	if db_mode == 1 {
		fmt.Println("Initialising DB")
		InitDb("jlukose:testdb@/tr")
	}
	go o.writeWorker()
	for {
		newWorkers := <-o.needWorker
		for i := 0; i < newWorkers; i++ {
			go o.startWorker()
		}

		// Disable dynamic scaling if workers poll fixed size
		if o.config.workers != 0 {
			return
		}
	}
}

func (o *HTTPOutput) writeWorker() {
	// fmt.Println("inside writeWorker")
	file, _ := os.Create("result.tsv")
	defer file.Close()
	writer := csv.NewWriter(file)
	defer writer.Flush()
	writer.Comma = '\t'
	writer.Write([]string{"STATUS", "START_TIME", "DURATION", "UNIX_TS"})
	for {
		data, _ := <-o.output_queue
		writer.Write(data)
		// writer.Write([]string{"hihi"})
		writer.Flush()

	}
}

func (o *HTTPOutput) startWorker() {

	//fmt.Println("inside start worker function")
	client := NewHTTPClient(o.address, &HTTPClientConfig{
		FollowRedirects:    o.config.redirectLimit,
		Debug:              o.config.Debug,
		OriginalHost:       o.config.OriginalHost,
		Timeout:            o.config.Timeout,
		ResponseBufferSize: o.config.BufferSize,
	})

	deathCount := 0

	atomic.AddInt64(&o.activeWorkers, 1)

	for {

		select {
		case data := <-o.queue:
			o.sendRequest(client, data)
			deathCount = 0
		case <-time.After(time.Millisecond * 100):
			// When dynamic scaling enabled workers die after 2s of inactivity
			if o.config.workers == 0 {
				deathCount++
			} else {
				continue
			}

			if deathCount > 20 {
				workersCount := atomic.LoadInt64(&o.activeWorkers)

				// At least 1 startWorker should be alive
				if workersCount != 1 {
					atomic.AddInt64(&o.activeWorkers, -1)
					return
				}
			}
		}
	}
}

func (o *HTTPOutput) Write(data []byte) (n int, err error) {
	if !isRequestPayload(data) {
		return len(data), nil
	}

	buf := make([]byte, len(data))
	copy(buf, data)

	o.queue <- buf

	if o.config.stats {
		o.queueStats.Write(len(o.queue))
	}

	if o.config.workers == 0 {
		workersCount := atomic.LoadInt64(&o.activeWorkers)

		if len(o.queue) > int(workersCount) {
			o.needWorker <- len(o.queue)
		}
	}

	return len(data), nil
}

func (o *HTTPOutput) Read(data []byte) (int, error) {
	resp := <-o.responses

	if Settings.debug {
		Debug("[OUTPUT-HTTP] Received response:", string(resp.payload))
	}

	header := payloadHeader(ReplayedResponsePayload, resp.uuid, resp.roundTripTime, resp.startedAt)
	copy(data[0:len(header)], header)
	copy(data[len(header):], resp.payload)

	return len(resp.payload) + len(header), nil
}

func InitDb(dataSourceName string) {

	var err error
	db, err = sql.Open("mysql", dataSourceName)
	if err != nil {
		panic(err.Error())
	}
	//defer db.Close()
	err = db.Ping()
	if err != nil {
		panic(err.Error())
	}

}

func WriteToDb(start_time string, duration string, status_code string) {
	fmt.Println("Writing to Db")
	var projectRunId int
	stmtOut := db.QueryRow("SELECT id FROM project_run ORDER BY ID DESC LIMIT 1")
	err := stmtOut.Scan(&projectRunId) // WHERE number = 1
	if err != nil {
		panic(err.Error()) // proper error handling instead of panic in your app
	}
	stmtIns, err := db.Prepare("INSERT INTO metric_store (reqStartTime, duration, statusCode, projectRunId ) VALUES( ?,?,?,? )") // ? = placeholder
	if err != nil {
		panic(err.Error()) // proper error handling instead of panic in your app
	}
	defer stmtIns.Close()
	_, err = stmtIns.Exec(start_time, duration, status_code, projectRunId) // Insert tuples (i, i^2)
	if err != nil {
		panic(err.Error()) // proper error handling instead of panic in your app
	}
	fmt.Printf("projectRunId : %d \n", projectRunId)
}

func (o *HTTPOutput) sendRequest(client *HTTPClient, request []byte) {
	meta := payloadMeta(request)
	if Settings.debug {
		Debug(meta)
	}

	if len(meta) < 2 {
		return
	}
	uuid := meta[1]

	body := payloadBody(request)
	if !proto.IsHTTPPayload(body) {
		return
	}

	start := time.Now()
	resp, err := client.Send(body)
	stop := time.Now()
	delta := stop.Sub(start)
	unix_time := strconv.FormatInt(start.UnixNano(), 10)
	duration := strconv.FormatInt(int64(delta/1000), 10)
	start_time := start.Format("2006-01-02 15:04:05.00000")
	// fmt.Printf("%v %v %v \n",string(resp[9:13]),duration,start_time)
	// fmt.Printf("Status_code : %v Duration : %v  Started at  : %v \n" , string(resp[9:13]), delta.Seconds() , start )
	// writer.Write([]string{strconv.FormatInt(int64(c), 16), string(resp[9:13]), duration, start_time })
	if db_mode == 1 {
		WriteToDb(start_time, duration, string(resp[9:13]))
	}
	o.output_queue <- []string{string(resp[9:13]), start_time, duration, unix_time}
	if err != nil {
		Debug("Request error:", err)
	}

	if o.config.TrackResponses {
		o.responses <- response{resp, uuid, start.UnixNano(), stop.UnixNano() - start.UnixNano()}
	}

	if o.elasticSearch != nil {
		o.elasticSearch.ResponseAnalyze(request, resp, start, stop)
	}
}

func (o *HTTPOutput) String() string {
	return "HTTP output: " + o.address
}
