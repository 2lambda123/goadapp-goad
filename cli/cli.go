package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/dustin/go-humanize"
	"github.com/goadapp/goad"
	"github.com/goadapp/goad/queue"
	"github.com/goadapp/goad/version"
	"github.com/naoina/toml"
	"github.com/nsf/termbox-go"
)

var (
	app              = kingpin.New("goad", "An AWS Lambda powered load testing tool")
	urlFlag          = app.Flag("url", "URL to load test").Short('u')
	url              = urlFlag.String()
	methodFlag       = app.Flag("method", "HTTP method").Short('m').Default("GET")
	method           = methodFlag.String()
	bodyFlag         = app.Flag("body", "HTTP request body").Short('b')
	body             = bodyFlag.String()
	concurrencyFlag  = app.Flag("concurrency", "Number of concurrent requests").Short('c').Default("10")
	concurrency      = concurrencyFlag.Int()
	requestsFlag     = app.Flag("requests", "Total number of requests to make").Short('n').Default("1000")
	requests         = requestsFlag.Int()
	timelimitFlag    = app.Flag("timelimit", "Seconds to max. to spend on benchmarking").Short('N').Default("3600")
	timelimit        = timelimitFlag.Int()
	timeoutFlag      = app.Flag("timeout", "Request timeout in seconds").Short('t').Default("15")
	timeout          = timeoutFlag.Int()
	regionsFlag      = app.Flag("region", "AWS regions to run in (repeat flag to run in more then one region)").Short('r')
	regions          = regionsFlag.Strings()
	awsProfileFlag   = app.Flag("awsprofile", "AWS named profile to use").Short('p')
	awsProfile       = awsProfileFlag.String()
	outputFileFlag   = app.Flag("output", "Optional path to JSON file for result storage").Short('o')
	outputFile       = outputFileFlag.String()
	headersFlag      = app.Flag("header", "HTTP request header (repeat flag to add more then one header)").Short('H')
	headers          = headersFlag.Strings()
	settingsFileFlag = app.Flag("settings", "Load settings from file (defaults to .goad)").Short('s')
	settingsFile     = settingsFileFlag.ExistingFile()
)

const coldef = termbox.ColorDefault
const nano = 1000000000

func main() {
	app.HelpFlag.Short('h')
	app.Version(version.Version)

	config := aggregateConfiguration()
	test := createGoadTest(config)

	var finalResult queue.RegionsAggData
	defer printSummary(&finalResult)

	if config.Output != "" {
		defer saveJSONSummary(*outputFile, &finalResult)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM) // but interrupts from kbd are blocked by termbox

	start(test, &finalResult, sigChan)
}

func aggregateConfiguration() *goad.TestConfig {
	cmdLineConfig := parseCommandline()
	if cmdLineConfig.Settings == "" {
		cmdLineConfig.Settings = "goad.ini"
	}
	config := parseSettingsFile(cmdLineConfig.Settings)
	applyDefaultsFromConfig(config)
	return parseCommandline()
}

func applyDefaultsFromConfig(config *goad.TestConfig) {
	applyDefaultIfNotZero(awsProfileFlag, config.AwsProfile)
	applyDefaultIfNotZero(bodyFlag, config.Body)
	applyDefaultIfNotZero(concurrencyFlag, prepareInt(config.Concurrency))
	applyDefaultIfNotZero(headersFlag, config.Headers)
	applyDefaultIfNotZero(methodFlag, config.Method)
	applyDefaultIfNotZero(outputFileFlag, config.Output)
	applyDefaultIfNotZero(regionsFlag, config.Regions)
	applyDefaultIfNotZero(requestsFlag, prepareInt(config.Requests))
	applyDefaultIfNotZero(timelimitFlag, prepareInt(config.Timelimit))
	applyDefaultIfNotZero(timeoutFlag, prepareInt(config.Timeout))
	applyDefaultIfNotZero(urlFlag, config.URL)
	if config.URL == "" {
		urlFlag.Required()
	}
	if len(config.Regions) == 0 {
		regionsFlag.Default("us-east-1", "eu-west-1", "ap-northeast-1")
	}
}

func applyDefaultIfNotZero(flag *kingpin.FlagClause, def interface{}) {
	value := reflect.ValueOf(def)
	kind := value.Kind()
	if isNotZero(value) {
		if kind == reflect.Slice || kind == reflect.Array {
			strs := make([]string, 0)
			for i := 0; i < value.Len(); i++ {
				strs = append(strs, value.Index(i).String())
			}
			flag.Default(strs...)
		} else {
			flag.Default(value.String())
		}
	}
}

func prepareInt(value int) string {
	if value == 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func isNotZero(v reflect.Value) bool {
	return !isZero(v)
}

func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Func, reflect.Map, reflect.Slice:
		return v.IsNil()
	case reflect.Array:
		z := true
		for i := 0; i < v.Len(); i++ {
			z = z && isZero(v.Index(i))
		}
		return z
	case reflect.Struct:
		z := true
		for i := 0; i < v.NumField(); i++ {
			z = z && isZero(v.Field(i))
		}
		return z
	}
	// Compare other types directly:
	z := reflect.Zero(v.Type())
	return v.Interface() == z.Interface()
}

func parseSettingsFile(file string) *goad.TestConfig {
	config := &goad.TestConfig{}
	f, err := os.Open(file)
	if err == nil {
		defer f.Close()
		if fail := toml.NewDecoder(f).Decode(&config); fail != nil {
			fmt.Printf("Error parsing settings file: %s\n", fail.Error())
			os.Exit(1)
		}
	}
	return config
}

func parseCommandline() *goad.TestConfig {
	args := os.Args[1:]

	_, err := app.Parse(args)
	if err != nil {
		fmt.Println(err.Error())
		app.Usage(args)
		os.Exit(1)
	}

	regionsArray := parseRegionsForBackwardsCompatibility(*regions)

	config := &goad.TestConfig{}
	config.URL = *url
	config.Concurrency = *concurrency
	config.Requests = *requests
	config.Timelimit = *timelimit
	config.Timeout = *timeout
	config.Regions = regionsArray
	config.Method = *method
	config.Body = *body
	config.Headers = *headers
	config.AwsProfile = *awsProfile
	config.Output = *outputFile
	config.Settings = *settingsFile
	return config
}

func parseRegionsForBackwardsCompatibility(regions []string) []string {
	parsedRegions := make([]string, 0)
	for _, str := range regions {
		parsedRegions = append(parsedRegions, strings.Split(str, ",")...)
	}
	return parsedRegions
}

func createGoadTest(config *goad.TestConfig) *goad.Test {
	test, err := goad.NewTest(config)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	return test
}

func start(test *goad.Test, finalResult *queue.RegionsAggData, sigChan chan os.Signal) {
	err := termbox.Init()
	if err != nil {
		panic(err)
	}

	defer test.Clean()
	defer termbox.Close()
	termbox.Sync()
	renderString(0, 0, "Launching on AWS... (be patient)", coldef, coldef)
	renderLogo()
	termbox.Flush()

	resultChan := test.Start()

	_, h := termbox.Size()
	renderString(0, h-1, "Press ctrl-c to interrupt", coldef, coldef)
	termbox.Flush()

	go func() {
		for {
			event := termbox.PollEvent()
			if event.Key == 3 {
				sigChan <- syscall.SIGINT
			}
		}
	}()

	startTime := time.Now()
	firstTime := true
outer:
	for {
		select {
		case result, ok := <-resultChan:
			if !ok {
				break outer
			}

			if firstTime {
				clearLogo()
				firstTime = false
			}

			// sort so that regions always appear in the same order
			var regions []string
			for key := range result.Regions {
				regions = append(regions, key)
			}
			sort.Strings(regions)
			y := 3
			totalReqs := 0
			for _, region := range regions {
				data := result.Regions[region]
				totalReqs += data.TotalReqs
				y = renderRegion(data, y)
				y++
			}

			y = 0
			var percentDone float64
			if result.TotalExpectedRequests > 0 {
				percentDone = float64(totalReqs) / float64(result.TotalExpectedRequests)
			} else {
				percentDone = math.Min(float64(time.Since(startTime).Seconds())/float64(test.Config.Timelimit), 1.0)
			}
			drawProgressBar(percentDone, y)

			termbox.Flush()
			finalResult.Regions = result.Regions

		case <-sigChan:
			break outer
		}
	}
}

func renderLogo() {
	s1 := `	  _____                 _`
	s2 := `  / ____|               | |`
	s3 := `	| |  __  ___   ____  __| |`
	s4 := `	| | |_ |/ _ \ / _  |/ _  |`
	s5 := `	| |__| | (_) | (_| | (_| |`
	s6 := `	 \_____|\___/ \__,_|\__,_|`
	s7 := " Global load testing with Go"
	arr := [...]string{s1, s2, s3, s4, s5, s6, s7}
	for i, str := range arr {
		renderString(0, i+1, str, coldef, coldef)
	}
}

// Also clears loading message
func clearLogo() {
	for i := 0; i < 8; i++ {
		renderString(0, i, "                                ", coldef, coldef)
	}
}

// renderRegion returns the y for the next empty line
func renderRegion(data queue.AggData, y int) int {
	x := 0
	renderString(x, y, "Region: ", termbox.ColorWhite, termbox.ColorBlue)
	x += 8
	regionStr := fmt.Sprintf("%s", data.Region)
	renderString(x, y, regionStr, termbox.ColorWhite|termbox.AttrBold, termbox.ColorBlue)
	x = 0
	y++
	headingStr := "   TotReqs   TotBytes    AvgTime   AvgReq/s  AvgKbps/s"
	renderString(x, y, headingStr, coldef|termbox.AttrBold, coldef)
	y++
	resultStr := fmt.Sprintf("%10d %10s   %7.3fs %10.2f %10.2f", data.TotalReqs, humanize.Bytes(uint64(data.TotBytesRead)), float64(data.AveTimeForReq)/nano, data.AveReqPerSec, data.AveKBytesPerSec)
	renderString(x, y, resultStr, coldef, coldef)
	y++
	headingStr = "   Slowest    Fastest   Timeouts  TotErrors"
	renderString(x, y, headingStr, coldef|termbox.AttrBold, coldef)
	y++
	resultStr = fmt.Sprintf("  %7.3fs   %7.3fs %10d %10d", float64(data.Slowest)/nano, float64(data.Fastest)/nano, data.TotalTimedOut, totErrors(&data))
	renderString(x, y, resultStr, coldef, coldef)
	y++

	return y
}

func totErrors(data *queue.AggData) int {
	var okReqs int
	for statusStr, value := range data.Statuses {
		status, _ := strconv.Atoi(statusStr)
		if status < 400 {
			okReqs += value
		}
	}
	return data.TotalReqs - okReqs
}

func drawProgressBar(percent float64, y int) {
	x := 0
	width := 52
	percentStr := fmt.Sprintf("%5.1f%%            ", percent*100)
	renderString(x, y, percentStr, coldef, coldef)
	y++
	hashes := int(percent * float64(width))
	if percent > 0.99 {
		hashes = width
	}
	renderString(x, y, "[", coldef, coldef)

	for x++; x <= hashes; x++ {
		renderString(x, y, "#", coldef, coldef)
	}
	renderString(width+1, y, "]", coldef, coldef)
}

func renderString(x int, y int, str string, f termbox.Attribute, b termbox.Attribute) {
	for i, c := range str {
		termbox.SetCell(x+i, y, c, f, b)
	}
}

func boldPrintln(msg string) {
	fmt.Printf("\033[1m%s\033[0m\n", msg)
}

func printData(data *queue.AggData) {
	boldPrintln("   TotReqs   TotBytes    AvgTime   AvgReq/s  AvgKbps/s")
	fmt.Printf("%10d %10s   %7.3fs %10.2f %10.2f\n", data.TotalReqs, humanize.Bytes(uint64(data.TotBytesRead)), float64(data.AveTimeForReq)/nano, data.AveReqPerSec, data.AveKBytesPerSec)
	boldPrintln("   Slowest    Fastest   Timeouts  TotErrors")
	fmt.Printf("  %7.3fs   %7.3fs %10d %10d", float64(data.Slowest)/nano, float64(data.Fastest)/nano, data.TotalTimedOut, totErrors(data))
	fmt.Println("")
}

func printSummary(result *queue.RegionsAggData) {
	if len(result.Regions) == 0 {
		boldPrintln("No results received")
		return
	}
	boldPrintln("Regional results")
	fmt.Println("")

	for region, data := range result.Regions {
		fmt.Println("Region: " + region)
		printData(&data)
	}

	overall := queue.SumRegionResults(result)

	fmt.Println("")
	boldPrintln("Overall")
	fmt.Println("")
	printData(overall)

	boldPrintln("HTTPStatus   Requests")
	for statusStr, value := range overall.Statuses {
		fmt.Printf("%10s %10d\n", statusStr, value)
	}
	fmt.Println("")
}

func saveJSONSummary(path string, result *queue.RegionsAggData) {
	if len(result.Regions) == 0 {
		return
	}
	results := make(map[string]queue.AggData)

	for region, data := range result.Regions {
		results[region] = data
	}

	overall := queue.SumRegionResults(result)

	results["overall"] = *overall
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Println(err)
		return
	}
	err = ioutil.WriteFile(path, b, 0644)
	if err != nil {
		fmt.Println(err)
		return
	}
}
