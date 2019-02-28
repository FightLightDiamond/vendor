package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/schema"

	"github.com/gorilla/websocket"
)

var gMyName = "spd-cli"
var gLogger *log.Logger

// Interface for sorting speed sample slice
type speedSampleSlice []int64

func (p speedSampleSlice) Len() int           { return len(p) }
func (p speedSampleSlice) Less(i, j int) bool { return p[i] < p[j] }
func (p speedSampleSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Speed test routine context data
type speedContext struct {
	totalTransferred int64       // number of total downloaded/uploaded bytes
	lastSampling     time.Time   // Last sampling time
	countMutex       *sync.Mutex // mutex to gard the totalDownloaded variable
	isPreparing      bool        // preparing status
	isTimedOut       bool        // timed-out status
	buffer           []byte      // temporary buffer
}

// Program configuration
type speedCfg struct {
	infoServer      string     // The server provide hosting information
	server          serverInfo // Server information
	serverAddress   string     // Server address, e.g. "202.166.127.121"
	dlStreamCnt     int        // Number of download streams
	ulStreamCnt     int        // Number of upload streams
	pingCount       int        // Number of pings to perform
	dlDuration      int        // Download time, in seconds
	ulDuration      int        // Upload time, in seconds
	prepareDuration int        // Preparation time, in seconds
	dlSize          int        // Sze of chunk sent by WebSocket download
	ulSize          int        // Size of chunk send by WebSocket upload
	dlPortFrom      int        // Download port from
	dlPortTo        int        // Download port to
	ulPortFrom      int        // Upload port from
	ulPortTo        int        // Upload port to
	pingPort        int        // Ping port
	maxMsgSize      int        // Max message size, in KB
	verbose         bool       // Verbose log
	verboseDebug    bool       // Verbose debug log
	samplingPeriod  int        // Sampling period, in milliseconds
	percentile      int        // Percentile
	country         string     // Country code
	jsonOutput      bool       // Json output
}

// Data structure stores the server informations deserialized from server
type serverInfo struct {
	ServerURL string `json:"server_url"`
	ServerID  int    `json:"server_id"`
	ClientIP  string `json:"client_ip"`
}

type serverInfoResponse struct {
	ErrorCode    int        `json:"code"`
	ErrorMessage string     `json:"message"`
	ServerInfo   serverInfo `json:"data"`
}

// Customize the float64 data type to only display 2 decimal places for json serialization
type speedNumber float64

func (n *speedNumber) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%.2f", *n)), nil
}

// Data type structure stores the speed test result verbosed to stdout
type speeds struct {
	Ping        speedNumber `json:"ping"`
	Download    speedNumber `json:"download"`
	Upload      speedNumber `json:"upload"`
	MaxDownload speedNumber `json:"-"` // Skip from json serialization
	MaxUpload   speedNumber `json:"-"` // Skip from json serialization
}

type speedResult struct {
	Status string `json:"status"`
	Result speeds `json:"result"`
}

// Data structure stores the speed test result to send to server
type speedLogResult struct {
	TestDate        string      `schema:"test_date"`
	IPAddress       string      `schema:"ip_address"`
	ServerID        int         `schema:"server_id"`
	CountryCode     string      `schema:"country_code"`
	OS              string      `schema:"os"`
	Browser         string      `schema:"browser"`
	ServiceProvider string      `schema:"service_provider"`
	Address         string      `schema:"address"`
	Result          int         `schema:"result"`
	DlSpeed         speedNumber `schema:"download_speed"`
	MaxDlSpeed      speedNumber `schema:"max_download_speed"`
	UlSpeed         speedNumber `schema:"upload_speed"`
	MaxUlSpeed      speedNumber `schema:"max_upload_speed"`
	Latency         speedNumber `schema:"latency"`
	Description     string      `schema:"description"`
}

const gStatusAborted = "aborted"
const gStatusSuccess = "success"
const gStatusError = "error"
const gStatusTimeout = "timeout"
const gErrorCtrlC = "CtrlC"

func main() {
	gLogger = log.New(os.Stderr, "["+gMyName+"] ", log.LstdFlags)

	// Getting program configuration via command line argument or via info server
	var cfg speedCfg
	getServerSucceeded := parseConfig(&cfg)

	if cfg.verboseDebug {
		cfg.verbose = true
	}

	// Validate the configuration
	if cfg.dlPortFrom > cfg.dlPortTo {
		gLogger.Fatal("Invalid download port range: ", cfg.dlPortFrom, "-", cfg.dlPortTo)
	}

	if cfg.ulPortFrom > cfg.ulPortTo {
		gLogger.Fatal("Invalid upload port range: ", cfg.ulPortFrom, "-", cfg.ulPortTo)
	}
	// More validations to be added

	var result speedResult
	if getServerSucceeded {
		doTest(&cfg, &result)
	} else {
		result.Status = gStatusError
	}

	// Output to screen
	if cfg.jsonOutput {
		jsonResult, err := json.Marshal(&result)
		if err != nil {
			gLogger.Printf("Failed to display json result, error: %s", err.Error())
			return
		}

		fmt.Println(string(jsonResult))
	}

	// Upload log
	if getServerSucceeded {
		postResult(&cfg, &result)
	}
}

// Perform the test and return result
func doTest(
	cfg *speedCfg,
	result *speedResult) {

	logSeparator := "-------------------------------------------"
	gLogger.Print(logSeparator)
	gLogger.Printf("Starting %s ...", gMyName)

	// Verbose the configuration
	if cfg.verbose {
		verboseCfg(cfg)
		gLogger.Print(logSeparator)
	}

	var dlSpeed float64
	var maxDlSpeed float64

	var ulSpeed float64
	var maxUlSpeed float64

	var pingResult float64

	pingResult, err := doPing(cfg)
	if err != nil {
		if err.Error() == gErrorCtrlC {
			result.Status = gStatusAborted
		} else {
			result.Status = gStatusTimeout
		}
		return
	}

	if pingResult >= 0.0 {
		gLogger.Printf("Ping: %.2f ms", pingResult)
		gLogger.Print(logSeparator)

		dlSpeed, maxDlSpeed, err = doTransfer(cfg, true)
		if err != nil {
			if err.Error() == gErrorCtrlC {
				result.Status = gStatusAborted
			} else {
				result.Status = gStatusTimeout
			}
			return
		}
		gLogger.Print("Download speed: ", displaySpeed(dlSpeed))
		gLogger.Print(logSeparator)

		ulSpeed, maxUlSpeed, err = doTransfer(cfg, false)
		if err != nil {
			if err.Error() == gErrorCtrlC {
				result.Status = gStatusAborted
			} else {
				result.Status = gStatusTimeout
			}
			return
		}
		gLogger.Print("Upload speed: ", displaySpeed(ulSpeed))
	} else {
		gLogger.Print("Failed to establish connection to server")
		result.Status = gStatusError
	}

	result.Status = gStatusSuccess
	result.Result.Ping = speedNumber(pingResult)                  // milliseconds
	result.Result.Download = speedNumber(dlSpeed * 8 / 1e6)       // Mbit/s
	result.Result.MaxDownload = speedNumber(maxDlSpeed * 8 / 1e6) // MBit/s
	result.Result.Upload = speedNumber(ulSpeed * 8 / 1e6)         // MBit.s
	result.Result.MaxDownload = speedNumber(maxUlSpeed * 8 / 1e6) // MBit/s
	gLogger.Print(logSeparator)
	gLogger.Printf("Finished!")
}

func verboseCfg(cfg *speedCfg) {
	gLogger.Print("Host address: ", cfg.serverAddress)
	gLogger.Printf("Download port range: %d-%d", cfg.dlPortFrom, cfg.dlPortTo)
	gLogger.Printf("Upload port range: %d-%d", cfg.ulPortFrom, cfg.ulPortTo)
	gLogger.Printf("Number of download streams: %d", cfg.dlStreamCnt)
	gLogger.Printf("Number of upload streams: %d", cfg.ulStreamCnt)
	gLogger.Printf("Preparation time: %d seconds", cfg.prepareDuration)
	gLogger.Printf("Download test time: %d seconds", cfg.dlDuration)
	gLogger.Printf("Upload test time: %d seconds", cfg.ulDuration)
	gLogger.Printf("Buffer size: %s", displaySize(int64(cfg.maxMsgSize)<<10))
}

// Post speed test result to the server
func postResult(cfg *speedCfg, result *speedResult) {
	t := time.Now()
	testDate := fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Day())
	testResult := 0
	if result.Status == gStatusSuccess {
		testResult = 1
	}

	logResult := speedLogResult{
		IPAddress:       cfg.server.ClientIP,
		TestDate:        testDate,
		ServerID:        cfg.server.ServerID,
		CountryCode:     cfg.country,
		OS:              runtime.GOOS,
		Browser:         "cli",
		ServiceProvider: "",
		Address:         cfg.serverAddress,
		Result:          testResult,
		DlSpeed:         result.Result.Download,
		MaxDlSpeed:      result.Result.MaxDownload,
		UlSpeed:         result.Result.Upload,
		MaxUlSpeed:      result.Result.Upload,
		Latency:         result.Result.Ping,
		Description:     "Speedtest cli",
	}

	param := url.Values{}
	encoder := schema.NewEncoder()
	err := encoder.Encode(&logResult, param)
	if err != nil {
		gLogger.Printf("Failed to serialize test result log, error: %s", err.Error())
		return
	}

	if cfg.verboseDebug {
		gLogger.Printf("Result: %s", param.Encode())
	}

	url, err := makeURL(cfg.infoServer, "/api/cli/saveTestLog")
	if err != nil {
		gLogger.Printf("Failed to make URL, error: %s", err.Error())
		return
	}

	if cfg.verboseDebug {
		gLogger.Printf("Saving test result to %s ...", url)
	}

	client := new(http.Client)
	resp, err := client.PostForm(url, param)
	if err != nil {
		gLogger.Printf("Failed to post data to server (%s), error: %s", url, err.Error())
		return
	}

	if resp.StatusCode != 200 {
		gLogger.Printf("Post result is responsed with status %d (%s)", resp.StatusCode, resp.Status)
	}
}

func parseConfig(cfg *speedCfg) bool {
	parseConfigFromArgs(cfg)

	// Try getting server address
	url, err := makeURL(cfg.infoServer, "/api/cli/getServerInfo/"+cfg.country)
	if err != nil {
		gLogger.Printf("Failed to make URL, error: %s", err.Error())
		return false
	}

	gLogger.Printf("Query speedtest server information from %s", url)

	serverResponse := querySpeedTestServer(url)
	if serverResponse == nil {
		return false
	}

	// Display error message if any
	if serverResponse.ErrorCode != 0 {
		gLogger.Print(serverResponse.ErrorMessage)
		return false
	}

	// Everything is OK, get the address from the server
	cfg.serverAddress = serverResponse.ServerInfo.ServerURL
	cfg.server = serverResponse.ServerInfo

	return true
}

func parseConfigFromArgs(cfg *speedCfg) {
	flag.StringVar(&cfg.infoServer, "s", "http://speed-portal.singnet.com.sg/", "Information server")
	flag.StringVar(&cfg.serverAddress, "addr", "202.166.127.121", "Server address")
	flag.IntVar(&cfg.dlPortFrom, "dlPortFrom", 8000, "First download port")
	flag.IntVar(&cfg.dlPortTo, "dlPortTo", 8009, "Last download port")
	flag.IntVar(&cfg.ulPortFrom, "ulPortFrom", 8100, "First upload port")
	flag.IntVar(&cfg.ulPortTo, "ulPortTo", 8109, "Last upload port")
	flag.IntVar(&cfg.pingPort, "pingPort", 8100, "Ping service port")

	flag.IntVar(&cfg.dlStreamCnt, "dlStreams", 10, "Number of concurrent download streams per port")
	flag.IntVar(&cfg.ulStreamCnt, "ulStreams", 3, "Number of concurrent upload streams per port")
	flag.IntVar(&cfg.pingCount, "pingCount", 5, "Ping count")

	flag.IntVar(&cfg.dlDuration, "dlTime", 10, "Download time, in seconds")
	flag.IntVar(&cfg.ulDuration, "ulTime", 10, "Upload time, in seconds")
	flag.IntVar(&cfg.prepareDuration, "preparationTime", 5, "Preparation time, in seconds")

	flag.IntVar(&cfg.maxMsgSize, "maxMsgSize", 1024, "Max message size in KB")

	flag.IntVar(&cfg.dlSize, "dlSize", 20, "Size of chunks sent by WebSocket download")
	flag.IntVar(&cfg.ulSize, "ulSize", 20, "Size of chunks sent by WebSocket upload")

	flag.IntVar(&cfg.samplingPeriod, "samplingPeriod", 500, "Sampling period, in milliseconds")
	flag.IntVar(&cfg.percentile, "percentile", 95, "Speed reuslt percentile")

	flag.BoolVar(&cfg.verbose, "verbose", false, "Verbose log")
	flag.BoolVar(&cfg.verboseDebug, "verboseDebug", false, "Verbose debug log (could slow down speed test)")

	flag.BoolVar(&cfg.jsonOutput, "j", false, "Display result in json format")
	flag.StringVar(&cfg.country, "c", "SG", "Country code")
	flag.Parse()
}

// Query for the test server information
func querySpeedTestServer(url string) *serverInfoResponse {
	resp, err := http.Get(url)
	if err != nil {
		gLogger.Printf("Failed to connect to information server (%s), error: %s", url, err.Error())
		return nil
	}

	if resp.StatusCode != 200 {
		gLogger.Printf("Failed to query information server, error: %d (%s)", resp.StatusCode, resp.Status)
		return nil
	}
	defer resp.Body.Close()
	var serverInfo *serverInfoResponse
	err = json.NewDecoder(resp.Body).Decode(&serverInfo)

	if err != nil {
		gLogger.Printf("Failed to parse response data, error: %s", err.Error())
		return nil
	}

	return serverInfo
}

// Display speed (Gbit/s, MBit/s, KBit/s or bit/s)
func displaySpeed(bytesPerSecond float64) string {
	bits := bytesPerSecond * 8
	kbs := bits / 1e3
	mbs := bits / 1e6
	gbs := bits / 1e9
	precisionFormat := "%.2f"

	if gbs >= 1.0 {
		return fmt.Sprintf(precisionFormat+" Gbit/s", gbs)
	} else if mbs >= 1.0 {
		return fmt.Sprintf(precisionFormat+" Mbit/s", mbs)
	} else if kbs >= 1.0 {
		return fmt.Sprintf(precisionFormat+" Kbit/s", kbs)
	}

	return fmt.Sprintf("%.2f bit/s", bits)
}

// Display size (GB, MB, KB or bytes)
func displaySize(s int64) string {
	if s >= (1 << 30) {
		return fmt.Sprintf("%d GB", s>>30)
	} else if s >= (1 << 20) {
		return fmt.Sprintf("%d MB", s>>20)
	} else if s >= (1 << 10) {
		return fmt.Sprintf("%d KB", s>>20)
	}
	return fmt.Sprintf("%d bytes", s)
}

// Ping the server
// Param:
//    cfg: configuration
// Return:
//    bool: whether we would connect to server
//    error: whether gErrorCtrlC has been pressed (nil if everything is fine)
func doPing(cfg *speedCfg) (float64, error) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	pingResult := make(chan float64, 1)

	host := fmt.Sprintf("%s:%d", cfg.serverAddress, cfg.pingPort)

	go pingRoutine(cfg, host, pingResult)
	select {
	case <-interrupt:
		return 0, errors.New(gErrorCtrlC)
	case result := <-pingResult:
		if result > 1.0 {
			result -= 1.0
		}
		return result, nil
	}
}

// Test downloading speed
// Params:
//    cfg: configuration
//    download: true to start downloading data, false to start uploading data
// Return:
//    float64: transfer speed (bytes per second)
//    float64: maximum transfer speed (bytes per second)
//    error: whether gErrorCtrlC has been pressed (nil if everything is fine)
func doTransfer(cfg *speedCfg, isDownload bool) (float64, float64, error) {
	var wgTransfer sync.WaitGroup
	var cntMutex sync.Mutex
	var speedSamples speedSampleSlice

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	ctx := speedContext{
		totalTransferred: 0,
		countMutex:       &cntMutex,
		isTimedOut:       false,
		isPreparing:      true}
	var transferType string
	var streamCnt int
	var startPort, endPort int
	var transferTime int

	// Setup speed test parameters
	if isDownload {
		transferType = "downloading"
		streamCnt = cfg.dlStreamCnt
		startPort, endPort = cfg.dlPortFrom, cfg.dlPortTo
		transferTime = cfg.dlDuration
	} else {
		transferType = "uploading"
		streamCnt = cfg.ulStreamCnt
		startPort, endPort = cfg.ulPortFrom, cfg.ulPortTo
		transferTime = cfg.ulDuration

		// Create a temporary buffer for uploading
		ulSize := cfg.ulSize << 13
		ctx.buffer = make([]byte, ulSize, ulSize)
		for i := 0; i < ulSize; i++ {
			ctx.buffer[i] = byte(rand.Intn(0x7F-0x20) + 0x20)
		}
	}
	gLogger.Printf("Start %s ...", transferType)

	// Create multiple transfering streams
	for i := 0; i < streamCnt; i++ {
		port := rand.Intn(endPort-startPort+1) + startPort
		host := fmt.Sprintf("%s:%d", cfg.serverAddress, port)
		if isDownload {
			go downloadRoutine(cfg, &wgTransfer, host, &ctx)
		} else {
			go uploadRoutine(cfg, &wgTransfer, host, &ctx)
		}

	}
	wgTransfer.Add(streamCnt)
	go func() {
		wgTransfer.Wait()
	}()

	select {
	case <-interrupt:
		// Ctrl-C pressed -> abort
		return 0, 0, errors.New(gErrorCtrlC)
	case <-time.After(time.Duration(cfg.prepareDuration) * time.Second):
		// Preperation ends, start measuring speed from now on
		if cfg.verbose {
			gLogger.Printf("Start measuring %s speed ...", transferType)
		}
		ctx.isPreparing = false
		ctx.lastSampling = time.Now()
	}

	// Sampling transfer speed
	t := time.NewTicker(time.Duration(cfg.samplingPeriod) * time.Millisecond)
	go func() {
		for _ = range t.C {
			ctx.countMutex.Lock()
			elapsed := time.Since(ctx.lastSampling)
			totalTransferred := float64(ctx.totalTransferred)
			ctx.countMutex.Unlock()

			delta := elapsed.Seconds()
			if delta > 0 {
				speed := totalTransferred / float64(delta)
				if cfg.verboseDebug {
					gLogger.Printf("Ticker time=[%.3f] %s=[%d] speed=[%.3f]",
						delta, transferType, int64(totalTransferred), speed)
				}
				speedSamples = append(speedSamples, int64(speed))
			}
		}
	}()

	select {
	case <-interrupt:
		return 0, 0, errors.New(gErrorCtrlC)
	case <-time.After(time.Duration(transferTime) * time.Second):
		// Trigger timed-out event
		if cfg.verbose {
			gLogger.Printf("%s finished", strings.Title(transferType))
		}
		ctx.isTimedOut = true
	}

	// Get the speed information and return result
	var perc float64
	if cfg.percentile <= 0 {
		perc = 1.0
	} else {
		// Speed in bytes/second
		perc = float64(cfg.percentile) / 100
	}

	_, maxSpeed, avgSpeed := minMaxAvgSpeed(speedSamples, perc)
	return maxSpeed, avgSpeed, nil
}

// the Go routine used for downloading data from server
func downloadRoutine(
	cfg *speedCfg,
	wg *sync.WaitGroup, /* sync object */
	serverAddress string, /* server address */
	ctx *speedContext) {
	defer wg.Done()

	// Try creating a connection to the server
	u := url.URL{Scheme: "ws", Host: serverAddress, Path: "/"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if cfg.verbose {
			gLogger.Print("Failed to initialize downloading socket: ", err)
		}
		return
	}

	defer conn.Close()
	conn.SetReadLimit((int64)(cfg.maxMsgSize << 10))
	if cfg.verboseDebug {
		gLogger.Printf("Preparing downloading from %s", serverAddress)
	}
	dlSizeBuffer := []byte(strconv.Itoa(cfg.dlSize))
	err = conn.WriteMessage(websocket.TextMessage, dlSizeBuffer)
	if err != nil {
		if cfg.verbose {
			gLogger.Println("Send data failed: ", err)
		}
		return
	}

	// Try downloading as much as we can until timed-out
	for {
		if ctx.isTimedOut {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if cfg.verbose {
				gLogger.Println("Downloading failed: ", err)
			}
			return
		}
		ln := len(message)
		// In preparing mode we should not count the downloaded bytes
		if !ctx.isPreparing {
			// Count the total downloaded data
			ctx.countMutex.Lock()
			ctx.totalTransferred += int64(ln)
			if cfg.verboseDebug {
				gLogger.Printf("Accumulating %d bytes", ctx.totalTransferred)
			}
			ctx.countMutex.Unlock()
		}
		if ln == 2 {
			err = conn.WriteMessage(websocket.TextMessage, dlSizeBuffer)
			if err != nil {
				if cfg.verbose {
					gLogger.Println("Send data failed: ", err)
				}
				return
			}
		}
	}
}

// the Go routine used for uploading data to server
func uploadRoutine(
	cfg *speedCfg,
	wg *sync.WaitGroup, /* sync object */
	serverAddress string, /* server address */
	ctx *speedContext) {
	defer wg.Done()
	// Try creating a connection to the server
	u := url.URL{Scheme: "ws", Host: serverAddress, Path: "/"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if cfg.verbose {
			gLogger.Print("Failed to initialize uploading socket: ", err)
		}
		return
	}

	defer conn.Close()
	conn.SetReadLimit((int64)(cfg.maxMsgSize << 10))
	if cfg.verboseDebug {
		gLogger.Printf("Preparing uploading to %s", serverAddress)
	}

	// Try uploading as much as we can until timed-out
	cnt := 64
	var okMessage = []byte("OK")

	for i := 0; i < cnt; i++ {
		if conn.WriteMessage(websocket.TextMessage, ctx.buffer) != nil {
			if cfg.verbose {
				gLogger.Println("Uploading failed failed: ", err)
			}
			return
		}
	}

	err = conn.WriteMessage(websocket.TextMessage, okMessage)
	if err != nil {
		if cfg.verbose {
			gLogger.Println("Uploading failed failed: ", err)
		}
		return
	}

	for {
		if ctx.isTimedOut {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if cfg.verbose {
				gLogger.Println("Reading data failed: ", err)
			}
			return
		}
		ln, err := strconv.Atoi(string(message))
		if err != nil {
			if cfg.verbose {
				gLogger.Println("Invalid return data from server ", err)
			}
			return
		}
		// In preparing mode we should not count the uploaded bytes
		if !ctx.isPreparing {
			// Count the total uploaded data
			ctx.countMutex.Lock()
			ctx.totalTransferred += int64(ln)
			if cfg.verboseDebug {
				gLogger.Printf("Accumulating %d bytes", ctx.totalTransferred)
			}
			ctx.countMutex.Unlock()
		}

		if ln == 2 {
			for i := 0; i < cnt; i++ {
				if conn.WriteMessage(websocket.TextMessage, ctx.buffer) != nil {
					if cfg.verbose {
						gLogger.Println("Uploading failed: ", err)
					}
					return
				}
			}
			err = conn.WriteMessage(websocket.TextMessage, okMessage)
			if err != nil {
				if cfg.verbose {
					gLogger.Println("Uploading failed failed: ", err)
				}
				return
			}
		}
	}
}

// the Go routine used for ping/pong
func pingRoutine(
	cfg *speedCfg,
	serverAddress string,
	pingResult chan float64) {
	// Try creating a connection to the server
	u := url.URL{Scheme: "ws", Host: serverAddress, Path: "/"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if cfg.verbose {
			gLogger.Print("Failed to initialize ping socket: ", err)
		}
		return
	}

	defer conn.Close()

	if cfg.verboseDebug {
		gLogger.Printf("Preparing pinging to %s", serverAddress)
	}

	// Try uploading as much as we can until timed-out
	var pingMessage = []byte("ping")
	var latency float64

	for i := 0; i < cfg.pingCount; i++ {
		t := time.Now()
		if conn.WriteMessage(websocket.TextMessage, pingMessage) != nil {
			if cfg.verbose {
				gLogger.Println("Uploading failed failed: ", err)
			}
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if cfg.verbose {
				gLogger.Println("Reading data failed: ", err)
			}
			return
		}
		delta := time.Since(t).Seconds()

		ln, err := strconv.Atoi(string(message))
		if err != nil {
			if cfg.verbose {
				gLogger.Println("Invalid return data from server ", err)
			}
			return
		}

		if ln != len(pingMessage) {
			if cfg.verbose {
				gLogger.Println("Invalid return data from server ", err)
			}
			return
		}

		latency = (latency*float64(i) + delta*1000.0) / float64(i+1)
	}
	pingResult <- latency
}

// Extract the average, minimum and maximum speed information
// Param:
//    values: array of samples to extract information
//    perc: percentile to get average value
// Return:
//    minimum speed value
//    maximum speed value
//    average speed value
func minMaxAvgSpeed(values speedSampleSlice, perc float64) (float64, float64, float64) {
	ps := []float64{perc}

	scores := make([]float64, len(ps))
	size := len(values)
	minValue, maxValue := 0.0, 0.0
	if size > 0 {
		sort.Sort(values)
		for i, p := range ps {
			pos := p * float64(size+1) //ALTERNATIVELY, DROP THE +1
			if pos < 1.0 {
				scores[i] = float64(values[0])
			} else if pos >= float64(size) {
				scores[i] = float64(values[size-1])
			} else {
				lower := float64(values[int(pos)-1])
				upper := float64(values[int(pos)])
				scores[i] = lower + (pos-math.Floor(pos))*(upper-lower)
			}
		}
		minValue = float64(values[0])
		maxValue = float64(values[size-1])
	}
	return minValue, maxValue, scores[0]
}

func makeURL(base string, path string) (string, error) {
	// We would NOT use path.join since it seems work for file system only
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if baseURL.Scheme == "" {
		baseURL.Scheme = "http"
		baseURL.Host = baseURL.Path
	}
	refURL, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	url := baseURL.ResolveReference(refURL)
	return url.String(), nil
}
