package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // Comment this line to disable pprof endpoint.
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/influxdata/telegraf/agent"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/internal/config"
	"github.com/influxdata/telegraf/internal/goplugin"
	"github.com/influxdata/telegraf/logger"
	_ "github.com/influxdata/telegraf/plugins/aggregators/all"
	"github.com/influxdata/telegraf/plugins/inputs"
	_ "github.com/influxdata/telegraf/plugins/inputs/all"
	"github.com/influxdata/telegraf/plugins/outputs"
	_ "github.com/influxdata/telegraf/plugins/outputs/all"
	_ "github.com/influxdata/telegraf/plugins/processors/all"
	"github.com/kardianos/service"
	//"telegraf/internal/config"
)

var fDebug = flag.Bool("debug", false,
	"turn on debug logging")
var fConfigServer = flag.String("configserver", "", "remote config server ip address")
var pprofAddr = flag.String("pprof-addr", "",
	"pprof address to listen on, not activate pprof if empty")
var fQuiet = flag.Bool("quiet", false,
	"run in quiet mode")
var fTest = flag.Bool("test", false, "enable test mode: gather metrics, print them out, and exit")
var fTestWait = flag.Int("test-wait", 0, "wait up to this many seconds for service inputs to complete in test mode")
var fConfig = flag.String("config", "", "configuration file to load")
var fConfigDirectory = flag.String("config-directory", "",
	"directory containing additional *.conf files")
var fVersion = flag.Bool("version", false, "display the version and exit")
var fSampleConfig = flag.Bool("sample-config", false,
	"print out full sample configuration")
var fPidfile = flag.String("pidfile", "", "file to write our pid to")
var fSectionFilters = flag.String("section-filter", "",
	"filter the sections to print, separator is ':'. Valid values are 'agent', 'global_tags', 'outputs', 'processors', 'aggregators' and 'inputs'")
var fInputFilters = flag.String("input-filter", "",
	"filter the inputs to enable, separator is :")
var fInputList = flag.Bool("input-list", false,
	"print available input plugins.")
var fOutputFilters = flag.String("output-filter", "",
	"filter the outputs to enable, separator is :")
var fOutputList = flag.Bool("output-list", false,
	"print available output plugins.")
var fAggregatorFilters = flag.String("aggregator-filter", "",
	"filter the aggregators to enable, separator is :")
var fProcessorFilters = flag.String("processor-filter", "",
	"filter the processors to enable, separator is :")
var fUsage = flag.String("usage", "",
	"print usage for a plugin, ie, 'telegraf --usage mysql'")
var fService = flag.String("service", "",
	"operate on the service (windows only)")
var fServiceName = flag.String("service-name", "telegraf", "service name (windows only)")
var fServiceDisplayName = flag.String("service-display-name", "Telegraf Data Collector Service", "service display name (windows only)")
var fRunAsConsole = flag.Bool("console", false, "run as console application (windows only)")
var fPlugins = flag.String("plugin-directory", "",
	"path to directory containing external plugins")

var (
	version string
	commit  string
	branch  string
)

var stop chan struct{}

func reloadLoop(
	stop chan struct{},
	inputFilters []string,
	outputFilters []string,
	aggregatorFilters []string,
	processorFilters []string,
) {
	reload := make(chan bool, 1)
	reload <- true
	for <-reload {
		reload <- false

		ctx, cancel := context.WithCancel(context.Background())

		signals := make(chan os.Signal)
		signal.Notify(signals, os.Interrupt, syscall.SIGHUP,
			syscall.SIGTERM, syscall.SIGINT)
		go func() {
			select {
			case sig := <-signals:
				if sig == syscall.SIGHUP {
					log.Printf("I! Reloading Telegraf config")
					<-reload
					reload <- true
				}
				cancel()
			case <-stop:
				cancel()
			}
		}()

		err := runAgent(ctx, inputFilters, outputFilters, signals)
		if err != nil && err != context.Canceled {
			log.Fatalf("E! [telegraf] Error running agent: %v", err)
		}
	}
}

func runAgent(ctx context.Context,
	inputFilters []string,
	outputFilters []string,
	signals chan<- os.Signal,
) error {
	log.Printf("I! Starting Telegraf %s", version)

	// If no other options are specified, load the config file and run.
	c := config.NewConfig()
	c.OutputFilters = outputFilters
	c.InputFilters = inputFilters
	err := c.LoadConfig(*fConfig)
	if err != nil {
		return err
	}

	if *fConfigDirectory != "" {
		err = c.LoadDirectory(*fConfigDirectory)
		if err != nil {
			return err
		}
	}
	if !*fTest && len(c.Outputs) == 0 {
		return errors.New("Error: no outputs found, did you provide a valid config file?")
	}
	if *fPlugins == "" && len(c.Inputs) == 0 {
		return errors.New("Error: no inputs found, did you provide a valid config file?")
	}

	if int64(c.Agent.Interval.Duration) <= 0 {
		return fmt.Errorf("Agent interval must be positive, found %s",
			c.Agent.Interval.Duration)
	}

	if int64(c.Agent.FlushInterval.Duration) <= 0 {
		return fmt.Errorf("Agent flush_interval must be positive; found %s",
			c.Agent.Interval.Duration)
	}

	ag, err := agent.NewAgent(c)
	if err != nil {
		return err
	}

	// Setup logging as configured.
	logConfig := logger.LogConfig{
		Debug:               ag.Config.Agent.Debug || *fDebug,
		Quiet:               ag.Config.Agent.Quiet || *fQuiet,
		LogTarget:           ag.Config.Agent.LogTarget,
		Logfile:             ag.Config.Agent.Logfile,
		RotationInterval:    ag.Config.Agent.LogfileRotationInterval,
		RotationMaxSize:     ag.Config.Agent.LogfileRotationMaxSize,
		RotationMaxArchives: ag.Config.Agent.LogfileRotationMaxArchives,
	}

	logger.SetupLogging(logConfig)

	if *fTest || *fTestWait != 0 {
		testWaitDuration := time.Duration(*fTestWait) * time.Second
		return ag.Test(ctx, testWaitDuration)
	}

	if *fConfigServer != "" {
		go CheckRemoteConfig(*fConfig, *fConfigServer, signals)
	}

	log.Printf("I! Loaded inputs: %s", strings.Join(c.InputNames(), " "))
	log.Printf("I! Loaded aggregators: %s", strings.Join(c.AggregatorNames(), " "))
	log.Printf("I! Loaded processors: %s", strings.Join(c.ProcessorNames(), " "))
	log.Printf("I! Loaded outputs: %s", strings.Join(c.OutputNames(), " "))
	log.Printf("I! Tags enabled: %s", c.ListTags())

	if *fPidfile != "" {
		f, err := os.OpenFile(*fPidfile, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("E! Unable to create pidfile: %s", err)
		} else {
			fmt.Fprintf(f, "%d\n", os.Getpid())

			f.Close()

			defer func() {
				err := os.Remove(*fPidfile)
				if err != nil {
					log.Printf("E! Unable to remove pidfile: %s", err)
				}
			}()
		}
	}

	return ag.Run(ctx)
}

func usageExit(rc int) {
	fmt.Println(internal.Usage)
	os.Exit(rc)
}

type program struct {
	inputFilters      []string
	outputFilters     []string
	aggregatorFilters []string
	processorFilters  []string
}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}
func (p *program) run() {
	stop = make(chan struct{})
	reloadLoop(
		stop,
		p.inputFilters,
		p.outputFilters,
		p.aggregatorFilters,
		p.processorFilters,
	)
}
func (p *program) Stop(s service.Service) error {
	close(stop)
	return nil
}

func formatFullVersion() string {
	var parts = []string{"Telegraf"}

	if version != "" {
		parts = append(parts, version)
	} else {
		parts = append(parts, "unknown")
	}

	if branch != "" || commit != "" {
		if branch == "" {
			branch = "unknown"
		}
		if commit == "" {
			commit = "unknown"
		}
		git := fmt.Sprintf("(git: %s %s)", branch, commit)
		parts = append(parts, git)
	}

	return strings.Join(parts, " ")
}

func main() {
	flag.Usage = func() { usageExit(0) }
	flag.Parse()
	args := flag.Args()

	sectionFilters, inputFilters, outputFilters := []string{}, []string{}, []string{}
	if *fSectionFilters != "" {
		sectionFilters = strings.Split(":"+strings.TrimSpace(*fSectionFilters)+":", ":")
	}
	if *fInputFilters != "" {
		inputFilters = strings.Split(":"+strings.TrimSpace(*fInputFilters)+":", ":")
	}
	if *fOutputFilters != "" {
		outputFilters = strings.Split(":"+strings.TrimSpace(*fOutputFilters)+":", ":")
	}

	aggregatorFilters, processorFilters := []string{}, []string{}
	if *fAggregatorFilters != "" {
		aggregatorFilters = strings.Split(":"+strings.TrimSpace(*fAggregatorFilters)+":", ":")
	}
	if *fProcessorFilters != "" {
		processorFilters = strings.Split(":"+strings.TrimSpace(*fProcessorFilters)+":", ":")
	}

	logger.SetupLogging(logger.LogConfig{})

	// Load external plugins, if requested.
	if *fPlugins != "" {
		log.Printf("I! Loading external plugins from: %s", *fPlugins)
		if err := goplugin.LoadExternalPlugins(*fPlugins); err != nil {
			log.Fatal("E! " + err.Error())
		}
	}

	if *pprofAddr != "" {
		go func() {
			pprofHostPort := *pprofAddr
			parts := strings.Split(pprofHostPort, ":")
			if len(parts) == 2 && parts[0] == "" {
				pprofHostPort = fmt.Sprintf("localhost:%s", parts[1])
			}
			pprofHostPort = "http://" + pprofHostPort + "/debug/pprof"

			log.Printf("I! Starting pprof HTTP server at: %s", pprofHostPort)

			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Fatal("E! " + err.Error())
			}
		}()
	}

	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Println(formatFullVersion())
			return
		case "config":
			config.PrintSampleConfig(
				sectionFilters,
				inputFilters,
				outputFilters,
				aggregatorFilters,
				processorFilters,
			)
			return
		}
	}

	// switch for flags which just do something and exit immediately
	switch {
	case *fOutputList:
		fmt.Println("Available Output Plugins: ")
		names := make([]string, 0, len(outputs.Outputs))
		for k := range outputs.Outputs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("  %s\n", k)
		}
		return
	case *fInputList:
		fmt.Println("Available Input Plugins:")
		names := make([]string, 0, len(inputs.Inputs))
		for k := range inputs.Inputs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("  %s\n", k)
		}
		return
	case *fVersion:
		fmt.Println(formatFullVersion())
		return
	case *fSampleConfig:
		config.PrintSampleConfig(
			sectionFilters,
			inputFilters,
			outputFilters,
			aggregatorFilters,
			processorFilters,
		)
		return
	case *fUsage != "":
		err := config.PrintInputConfig(*fUsage)
		err2 := config.PrintOutputConfig(*fUsage)
		if err != nil && err2 != nil {
			log.Fatalf("E! %s and %s", err, err2)
		}
		return
	}

	shortVersion := version
	if shortVersion == "" {
		shortVersion = "unknown"
	}

	// Configure version
	if err := internal.SetVersion(shortVersion); err != nil {
		log.Println("Telegraf version already configured to: " + internal.Version())
	}

	if runtime.GOOS == "windows" && windowsRunAsService() {
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" { // Should never happen
			programFiles = "C:\\Program Files"
		}
		svcConfig := &service.Config{
			Name:        *fServiceName,
			DisplayName: *fServiceDisplayName,
			Description: "Collects data using a series of plugins and publishes it to" +
				"another series of plugins.",
			Arguments: []string{"--config", programFiles + "\\Telegraf\\telegraf.conf"},
		}

		prg := &program{
			inputFilters:      inputFilters,
			outputFilters:     outputFilters,
			aggregatorFilters: aggregatorFilters,
			processorFilters:  processorFilters,
		}
		s, err := service.New(prg, svcConfig)
		if err != nil {
			log.Fatal("E! " + err.Error())
		}
		// Handle the --service flag here to prevent any issues with tooling that
		// may not have an interactive session, e.g. installing from Ansible.
		if *fService != "" {
			if *fConfig != "" {
				svcConfig.Arguments = []string{"--config", *fConfig}
			}
			if *fConfigDirectory != "" {
				svcConfig.Arguments = append(svcConfig.Arguments, "--config-directory", *fConfigDirectory)
			}
			//set servicename to service cmd line, to have a custom name after relaunch as a service
			svcConfig.Arguments = append(svcConfig.Arguments, "--service-name", *fServiceName)

			err := service.Control(s, *fService)
			if err != nil {
				log.Fatal("E! " + err.Error())
			}
			os.Exit(0)
		} else {
			winlogger, err := s.Logger(nil)
			if err == nil {
				//When in service mode, register eventlog target andd setup default logging to eventlog
				logger.RegisterEventLogger(winlogger)
				logger.SetupLogging(logger.LogConfig{LogTarget: logger.LogTargetEventlog})
			}
			err = s.Run()

			if err != nil {
				log.Println("E! " + err.Error())
			}
		}
	} else {
		stop = make(chan struct{})
		reloadLoop(
			stop,
			inputFilters,
			outputFilters,
			aggregatorFilters,
			processorFilters,
		)
	}
}

// Return true if Telegraf should create a Windows service.
func windowsRunAsService() bool {
	if *fService != "" {
		return true
	}

	if *fRunAsConsole {
		return false
	}

	return !service.Interactive()
}

func GetRemoteConfig(Address string, clientname string, clientip string) (result string) {
	resp, err := http.Get("http://" + Address + "/config/config?clientname=" + clientname + "&clientip=" + clientip)
	if err != nil {
		fmt.Println(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	strbody := string(body)
	fmt.Println("body is:", strbody)
	return strbody
}

func WriteConfig(config string, configdata string) (err error) {
	message := []byte(configdata)
	err = ioutil.WriteFile(config, message, 0644)
	if err != nil {
		fmt.Println(err)
		return err
	}
	return nil
}

type ConfigObject struct {
	ClientIp    string `json:"clientip"`
	ClientName  string `json:"clientname"`
	ProjectName string `json:"projectname"`
	Config      string `json:"config"`
}

type ConfigHash struct {
	ClientIp   string `json:"clientip"`
	ClientName string `json:"clientname"`
	Hash       []byte `json:"hash"`
}

func PostConfig(configserver string, config string, clientname string, clientip string) (Result string) {
	configdata, _ := ioutil.ReadFile(config)
	c := new(ConfigObject)
	c.Config = string(configdata)
	c.ClientIp = clientip
	c.ClientName = clientname
	c.ProjectName = ""
	data, _ := json.Marshal(c)
	url := "http://" + configserver + "/config/config"
	request, _ := http.NewRequest("POST", url, bytes.NewBuffer(data))
	request.Header.Add("content-type", "application/json")
	defer request.Body.Close()
	//client := new(http.Client)
	client := &http.Client{}
	response, _ := client.Do(request)
	body, _ := ioutil.ReadAll(response.Body)
	Result = string(body)
	fmt.Println(Result)

	return Result
}

func SendHash(Address string, clientip string, clientname string, hash []byte) (Result string) {
	client := &http.Client{}
	Hash := new(ConfigHash)
	Hash.ClientIp = clientip
	Hash.ClientName = clientname
	Hash.Hash = hash
	Hash_json, _ := json.Marshal(Hash)
	request, err := http.NewRequest("POST", "http://"+Address+"/config/hash", bytes.NewBuffer(Hash_json))
	if err != nil {
		fmt.Println("create new request failed")
		fmt.Println(err)
	}
	request.Header.Add("If-None-Match", `W/"wyzzy"`)
	response, err := client.Do(request)
	if err != nil {
		fmt.Println("get response failed ")
	}
	//rehash := new(RemoteHash)
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		fmt.Println("get body failed")
	}
	//json.Unmarshal(body, rehash)
	//Result = rehash.Hash
	Result = string(body)
	return Result
}

func RestartTelegraf(signals chan<- os.Signal) {
	fmt.Println("trying to restart telegraf")
	signals <- syscall.SIGHUP
}

func LocalHash(config string) (Result []byte) {
	configdata, _ := ioutil.ReadFile(config)
	//data = string(configdata)
	Md5Inst := md5.New()
	Md5Inst.Write(configdata)
	Result = Md5Inst.Sum([]byte(""))
	fmt.Printf("client Hash is %x\n", Result)
	return Result
}

func CheckRemoteConfig(config string, configserver string, signals chan<- os.Signal) {
	for {
		//	configserver := c.Tags["configserver"]

		lohash := LocalHash(config) //local config hash

		//get clientname
		clientname, _ := os.Hostname()

		// get clientip
		var clientip string
		interfacess, _ := net.Interfaces()
		for _, j := range interfacess {
			if j.Name == "eth0" {
				add, _ := j.Addrs()
				address := add[0].String()
				clientip = strings.Split(address, "/")[0]
			}
		}
		response := SendHash(configserver, clientip, clientname, lohash) //remote config  hash
		fmt.Println(response)
		switch response {
		case "0":
			//test0
			fmt.Println("no change")
		case "1":
			//test1
			fmt.Println("server change")
			configdata := GetRemoteConfig(configserver, clientname, clientip)

			fmt.Println(configdata)
			WriteConfig(config, configdata)
			RestartTelegraf(signals)
		case "2":
			//test2
			fmt.Println("client change")
			PostConfig(configserver, config, clientname, clientip)
		}
		time.Sleep(time.Duration(1) * time.Minute)

		// get config for get remote config server's addr
	}

}
