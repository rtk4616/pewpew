package pewpew

import (
	"bytes"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	reggen "github.com/lucasjones/reggen"
	http2 "golang.org/x/net/http2"
)

//so concurrent workers don't interlace messages
var writeLock sync.Mutex

type workerDone struct{}

type requestStat struct {
	Proto     string
	URL       string
	Method    string
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	//equivalent to the difference between StartTime and EndTime
	Duration time.Duration `json:"duration"`
	//HTTP Status Code, e.g. 200, 404, 503
	StatusCode      int   `json:"statusCode"`
	Error           error `json:"error"`
	DataTransferred int   //bytes
}

type (
	//Stress is the top level struct that contains the configuration of stress test
	StressConfig struct {
		Targets            []Target
		Verbose            bool
		Quiet              bool
		NoHTTP2            bool
		EnforceSSL         bool
		ResultFilenameJSON string
		ResultFilenameCSV  string

		//global target settings

		Count        int
		Concurrency  int
		Timeout      string
		Method       string
		Body         string
		BodyFilename string
		Headers      string
		UserAgent    string
		BasicAuth    string
		Compress     bool
		KeepAlive    bool
	}
	Target struct {
		URL          string
		RegexURL     bool
		Count        int //how many total requests to make
		Concurrency  int
		Timeout      string
		Method       string
		Body         string
		BodyFilename string
		Headers      string
		UserAgent    string
		BasicAuth    string
		Compress     bool
		KeepAlive    bool
	}
)

//defaults
var DefaultURL = "http://localhost"

const (
	DefaultCount       = 10
	DefaultConcurrency = 1
	DefaultTimeout     = "10s"
	DefaultMethod      = "GET"
	DefaultUserAgent   = "pewpew"
)

//NewStress creates a new Stress object
//with reasonable defaults, but needs URL set
func NewStressConfig() (s *StressConfig) {
	s = &StressConfig{
		Targets: []Target{
			{
				URL:         DefaultURL,
				Count:       DefaultCount,
				Concurrency: DefaultConcurrency,
				Timeout:     DefaultTimeout,
				Method:      DefaultMethod,
				UserAgent:   DefaultUserAgent,
			},
		},
	}
	return
}

//RunStress starts the stress tests
func RunStress(s StressConfig) error {
	err := ValidateTargets(s)
	if err != nil {
		fmt.Println(err.Error())
		return errors.New("invalid configuration")
	}
	targetCount := len(s.Targets)

	//setup the queue of requests, one queue per target
	requestQueues := make([](chan http.Request), targetCount)
	for idx, target := range s.Targets {
		requestQueues[idx] = make(chan http.Request, target.Count)
		for i := 0; i < target.Count; i++ {
			req, err := buildRequest(target)
			if err != nil {
				fmt.Println(err.Error())
				return errors.New("failed to create request with target configuration")
			}
			requestQueues[idx] <- req
		}
		close(requestQueues[idx])
	}

	if targetCount == 1 {
		fmt.Printf("Stress testing %d target:\n", targetCount)
	} else {
		fmt.Printf("Stress testing %d targets:\n", targetCount)
	}

	//when a target is finished, send all stats into this
	targetStats := make(chan []requestStat)
	for idx, target := range s.Targets {
		go func(target Target, requestQueue chan http.Request, targetStats chan []requestStat) {
			fmt.Printf("- Running %d tests at %s, %d at a time\n", target.Count, target.URL, target.Concurrency)

			workerDoneChan := make(chan workerDone)   //workers use this to indicate they are done
			requestStatChan := make(chan requestStat) //workers communicate each requests' info

			tr := &http.Transport{}
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: !s.EnforceSSL}
			tr.DisableCompression = !target.Compress
			tr.DisableKeepAlives = !target.KeepAlive
			if s.NoHTTP2 {
				tr.TLSNextProto = make(map[string](func(string, *tls.Conn) http.RoundTripper))
			} else {
				http2.ConfigureTransport(tr)
			}
			var timeout time.Duration
			if target.Timeout != "" {
				timeout, _ = time.ParseDuration(target.Timeout)
			} else {
				timeout = time.Duration(0)
			}
			client := &http.Client{Timeout: timeout, Transport: tr}

			//start up the workers
			for i := 0; i < target.Concurrency; i++ {
				go func() {
					for {
						select {
						case req, ok := <-requestQueue:
							if !ok {
								//queue is empty
								workerDoneChan <- workerDone{}
								return
							}

							response, stat := runRequest(req, client)
							if !s.Quiet {
								writeLock.Lock()
								printStat(stat)
								if s.Verbose {
									printVerbose(&req, response)
								}
								writeLock.Unlock()
							}

							requestStatChan <- stat
						}
					}
				}()
			}
			requestStats := make([]requestStat, target.Count)
			requestsCompleteCount := 0
			workersDoneCount := 0
			//wait for all workers to finish
			for {
				select {
				case <-workerDoneChan:
					workersDoneCount++
				case stat := <-requestStatChan:
					requestStats[requestsCompleteCount] = stat
					requestsCompleteCount++
				}
				if workersDoneCount == target.Concurrency {
					//all workers are finished
					break
				}
			}
			targetStats <- requestStats
		}(target, requestQueues[idx], targetStats)
	}
	targetRequestStats := make([][]requestStat, targetCount)
	targetDoneCount := 0
	for {
		select {
		case reqStats := <-targetStats:
			targetRequestStats[targetDoneCount] = reqStats
			targetDoneCount++
		}
		if targetDoneCount == targetCount {
			//all targets are finished
			break
		}
	}

	fmt.Print("\n----Summary----\n\n")

	//only print individual target data if multiple targets
	if len(s.Targets) > 1 {
		for idx, target := range s.Targets {
			//info about the request
			fmt.Printf("----Target %d: %s %s\n", idx+1, target.Method, target.URL)
			reqStats := createRequestsStats(targetRequestStats[idx])
			fmt.Println(createTextSummary(reqStats))
		}
	}

	//combine individual targets to a total one
	globalStats := []requestStat{}
	for i := range s.Targets {
		for j := range targetRequestStats[i] {
			globalStats = append(globalStats, targetRequestStats[i][j])
		}
	}
	if len(s.Targets) > 1 {
		fmt.Println("----Global----")
	}
	reqStats := createRequestsStats(globalStats)
	fmt.Println(createTextSummary(reqStats))

	//write out json
	if s.ResultFilenameJSON != "" {
		fmt.Print("Writing full result data to: " + s.ResultFilenameJSON + " ...")
		json, _ := json.MarshalIndent(globalStats, "", "    ")
		err = ioutil.WriteFile(s.ResultFilenameJSON, json, 0644)
		if err != nil {
			return errors.New("failed to write full result data to " +
				s.ResultFilenameJSON + ": " + err.Error())
		}
		fmt.Println("finished!")
	}
	//write out csv
	if s.ResultFilenameCSV != "" {
		fmt.Print("Writing full result data to: " + s.ResultFilenameCSV + " ...")
		file, err := os.Create(s.ResultFilenameCSV)
		if err != nil {
			return errors.New("failed to write full result data to " +
				s.ResultFilenameCSV + ": " + err.Error())
		}
		defer file.Close()

		writer := csv.NewWriter(file)

		for _, req := range globalStats {
			line := []string{
				req.StartTime.String(),
				fmt.Sprintf("%d", req.Duration),
				fmt.Sprintf("%d", req.StatusCode),
				fmt.Sprintf("%d bytes", req.DataTransferred),
			}
			err := writer.Write(line)
			if err != nil {
				return errors.New("failed to write full result data to " +
					s.ResultFilenameCSV + ": " + err.Error())
			}
		}
		defer writer.Flush()
		fmt.Println("finished!")
	}
	return nil
}

func ValidateTargets(s StressConfig) error {
	if len(s.Targets) == 0 {
		return errors.New("zero targets")
	}
	for _, target := range s.Targets {
		//checks
		if target.URL == "" {
			return errors.New("empty URL")
		}
		if target.Count <= 0 {
			return errors.New("request count must be greater than zero")
		}
		if target.Concurrency <= 0 {
			return errors.New("concurrency must be greater than zero")
		}
		if target.Timeout != "" {
			//TODO should save this parsed duration so don't have to inefficiently reparse later
			timeout, err := time.ParseDuration(target.Timeout)
			if err != nil {
				fmt.Println(err)
				return errors.New("failed to parse timeout: " + target.Timeout)
			}
			if timeout <= time.Millisecond {
				return errors.New("timeout must be greater than one millisecond")
			}
		}
		if target.Concurrency > target.Count {
			return errors.New("concurrency must be higher than request count")
		}
	}
	return nil
}

//build the http request out of the target's config
func buildRequest(t Target) (http.Request, error) {
	var urlStr string
	var err error
	//when regex set, generate urls
	if t.RegexURL {
		urlStr, err = reggen.Generate(t.URL, 10)
		if err != nil {
			return http.Request{}, errors.New("failed to parse regex: " + err.Error())
		}
	} else {
		urlStr = t.URL
	}
	URL, err := url.Parse(urlStr)
	if err != nil {
		return http.Request{}, errors.New("failed to parse URL " + urlStr + " : " + err.Error())
	}
	//default to http if not specified
	if URL.Scheme == "" {
		URL.Scheme = "http"
	}

	//setup the request
	var req *http.Request
	if t.BodyFilename != "" {
		fileContents, err := ioutil.ReadFile(t.BodyFilename)
		if err != nil {
			return http.Request{}, errors.New("failed to read contents of file " + t.BodyFilename + ": " + err.Error())
		}
		req, err = http.NewRequest(t.Method, URL.String(), bytes.NewBuffer(fileContents))
	} else if t.Body != "" {
		req, err = http.NewRequest(t.Method, URL.String(), bytes.NewBuffer([]byte(t.Body)))
	} else {
		req, err = http.NewRequest(t.Method, URL.String(), nil)
	}
	if err != nil {
		return http.Request{}, errors.New("failed to create request: " + err.Error())
	}
	//add headers
	if t.Headers != "" {
		headerMap, err := parseKeyValString(t.Headers, ",", ":")
		if err != nil {
			fmt.Println(err)
			return http.Request{}, errors.New("could not parse headers")
		}
		for key, val := range headerMap {
			req.Header.Add(key, val)
		}
	}

	req.Header.Set("User-Agent", t.UserAgent)

	if t.BasicAuth != "" {
		authMap, err := parseKeyValString(t.BasicAuth, ",", ":")
		if err != nil {
			fmt.Println(err)
			return http.Request{}, errors.New("could not parse basic auth")
		}
		for key, val := range authMap {
			req.SetBasicAuth(key, val)
			break
		}
	}
	return *req, nil
}

//splits on delim into parts and trims whitespace
//delim1 splits the pairs, delim2 splits amongst the pairs
//like parseKeyValString("key1: val2, key3 : val4,key5:key6 ", ",", ":") becomes
//["key1"]->"val2"
//["key3"]->"val4"
//["key5"]->"val6"
func parseKeyValString(keyValStr, delim1, delim2 string) (map[string]string, error) {
	m := make(map[string]string)
	pairs := strings.SplitN(keyValStr, delim1, -1)
	for _, pair := range pairs {
		parts := strings.SplitN(pair, delim2, 2)
		if len(parts) != 2 {
			return m, fmt.Errorf("failed to parse into two parts")
		}
		key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if key == "" || val == "" {
			return m, fmt.Errorf("key or value is empty")
		}
		m[key] = val
	}
	return m, nil
}
