// Wago (Watch, Go)
// A general purpose watch / build development tool.

// TODO: catch SIGINT and reset term
// see https://askubuntu.com/questions/171449/shell-does-not-show-typed-in-commands-reset-works-but-what-happened

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"

	"golang.org/x/net/http2"

	"github.com/JonahBraun/dog"
	"github.com/howeyc/fsnotify"
)

const VERSION = "1.2.0"

var (
	log     = dog.NewDog(dog.DEBUG)
	verbose = flag.Bool("v", false, "Verbose")
	quiet   = flag.Bool("q", false, "Quiet, only warnings and errors")

	buildCmd      = flag.String("cmd", "", "Run command, wait for it to complete.")
	daemonCmd     = flag.String("daemon", "", "Run command and leave running in the background.")
	daemonTimer   = flag.Int("timer", 0, "Wait miliseconds after starting daemon, then continue.")
	daemonTrigger = flag.String("trigger", "", "Wait for daemon to output this string, then continue.")
	exitWait      = flag.Int("exitwait", 50, "Max miliseconds a process has after a SIGTERM to exit before a SIGKILL.")
	fiddle        = flag.Bool("fiddle", false, "CLI fiddle mode! Start a web server, open browser to URL of targetDir/index.html")
	postCmd       = flag.String("pcmd", "", "Run command after daemon starts. Use this to kick off your test suite.")
	recursive     = flag.Bool("recursive", true, "Watch directory tree recursively.")
	targetDir     = flag.String("dir", "", "Directory to watch, defaults to current.")
	url           = flag.String("url", "", "Open browser to this URL after all commands are successful.")
	watchRegex    = flag.String("watch", `/[^\.][^/]*": (CREATE|MODIFY$)`, "React to FS events matching regex. Use -v to see all events.")
	ignoreRegex   = flag.String("ignore", `\.(git|hg|svn)`, "Ignore directories matching regex.")
	httpPort      = flag.String("http", "", "Start a HTTP server on this port, e.g. :8420")
	http2Port     = flag.String("h2", "", "Start a HTTP/TLS server on this port, e.g. :8421")
	keyFile       = flag.String("key", "", "X.509 key file for HTTP2/TLS, eg: key.pem")
	certFile      = flag.String("cert", "", "X.509 cert file for HTTP2/TLS, eg: cert.pem")
	webRoot       = flag.String("webroot", "", "Local directory to use as root for web server, defaults to -dir.")
	shell         = flag.String("shell", "", "Shell used to run commands, defaults to $SHELL, fallback to /bin/sh")
)

type Watcher struct {
	Event chan fmt.Stringer
	Error chan error
}

func main() {
	// the following function calls merely serve to logically organize what
	// is otherwise a VERY lengthy setup

	// TODO: have configSetup return a config object so that the reliance on
	// config globals is removed
	configSetup()

	startWebServer()

	ManageStdin()

	runChain(newWatcher(), catchSignals())
}

func catchSignals() chan struct{} {
	// quit needs to inform multiple receivers, sig can't do that
	quit := make(chan struct{})
	sig := make(chan os.Signal, 1)

	// TODO add SIGTERM to this (need OS conditional)
	signal.Notify(sig, os.Interrupt, os.Kill)

	go func() {
		<-sig
		close(quit)
	}()

	return quit
}

// event loop and action chain happen here
func runChain(watcher *Watcher, quit chan struct{}) {
	chain := make([]Runnable, 0, 5)

	// build chain of runnables
	if len(*buildCmd) > 0 {
		chain = append(chain, NewRunWait(*buildCmd))
	}
	if len(*daemonCmd) > 0 {
		if len(*daemonTrigger) > 0 {
			chain = append(chain, NewDaemonTrigger(*daemonCmd, *daemonTrigger))
		} else {
			chain = append(chain, NewDaemonTimer(*daemonCmd, *daemonTimer))
		}
	}
	if len(*postCmd) > 0 {
		chain = append(chain, NewRunWait(*postCmd))
	}
	if *url != "" {
		chain = append(chain, NewBrowser(*url))
	}

	eventRegex, err := regexp.Compile(*watchRegex)
	if err != nil {
		log.Fatal("Watch regex compile error:", err)(1)
	}

	var wg sync.WaitGroup

	// main loop
	for {
		// kill is passed to all Runnable so they know when they should exit
		kill := make(chan struct{})

		var drain func()
		drain = func() {
			select {
			case ev := <-watcher.Event:
				log.Debug("Extra event ignored:", ev.String())
				drain()
			default:
			}
		}
		drain()

		// event loop
		go func() {
			for {
				select {
				case ev := <-watcher.Event:
					if eventRegex.MatchString(ev.String()) {
						log.Info("Matched event:", ev.String())
						close(kill)
						return
					} else {
						log.Debug("Ignored event:", ev.String())
					}
				case err = <-watcher.Error:
					log.Fatal("Watcher error:", err)(5)
				case <-quit:
					close(kill)
					return
				}
			}
		}()

	RunLoop:
		for _, runnable := range chain {
			done, dead := runnable(kill)
			wg.Add(1)

			go func() {
				<-dead
				wg.Done()
			}()

			select {
			case d := <-done:
				if !d {
					// Runnable's success metric failed, break out of the chain
					break RunLoop
				}
			case <-kill:
				break RunLoop
			}
		}

		// ensure an event has occured, we may be here because all runnables completed
		<-kill

		// ensure all runnables (procs) are dead before restarting the chain
		wg.Wait()

		// check if we should quit
		select {
		case <-quit:
			log.Debug("Quitting main event/action loop")
			return
		default:
		}
	}
}

func newWatcher() *Watcher {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}

	ignore, err := regexp.Compile(*ignoreRegex)
	if err != nil {
		log.Fatal("Ignore regex compile error:", err)(1)
	}

	watchDir := func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			return nil
		}

		if err != nil {
			log.Err("Error reading dir, skipping:", path, err)
			return filepath.SkipDir
		}

		if ignore.MatchString(path) {
			log.Debug("Ignoring dir:", path)
			return filepath.SkipDir
		}

		log.Debug("Watching dir:", path)
		err = watcher.Watch(path)
		if err != nil {
			log.Err("Error watching dir (path, error):", path, err)
		}

		return nil
	}

	if *recursive == true {
		// errors are handled in watchDir
		filepath.Walk(*targetDir, watchDir)
	} else {
		err = watcher.Watch(*targetDir)
		if err != nil {
			log.Fatal("Error watching dir (path, error):", *targetDir, err)(1)
		}
	}

	// To facilitate testing (which sends artifical events from a timer),
	// we have an abstracted struct Watcher that holds the applicable channels.
	// fsnotify.FileEvent is a fmt.Stringer, but channels cannot be converted.
	// Unfortunately, an extra channel is necessary to perform the conversion.
	event := make(chan fmt.Stringer)
	go func() {
		for {
			event <- <-watcher.Event
		}
	}()

	return &Watcher{event, watcher.Error}
}

func startWebServer() {
	var err error

	if *webRoot == "" {
		*webRoot = *targetDir
	}

	if *httpPort != "" {
		log.Info("HTTP port", *httpPort)

		s := &http.Server{
			Addr:    *httpPort,
			Handler: http.FileServer(http.Dir(*webRoot)),
		}

		http2.ConfigureServer(s, nil)

		go func() {
			err := s.ListenAndServe()
			if err != nil {
				log.Fatal("HTTP server error:", err)(2)
			}
		}()
	}

	if *http2Port != "" {
		log.Info("HTTP2 & TLS port", *http2Port)

		var key, cert []byte
		if *keyFile == "" {
			key = []byte(x509Key)
			cert = []byte(x509Cert)
		} else {
			key, err = ioutil.ReadFile(*keyFile)
			if err != nil {
				log.Fatal(err)(15)
			}
			cert, err = ioutil.ReadFile(*certFile)
			if err != nil {
				log.Fatal(err)(15)
			}
		}

		tlsPair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			log.Fatal(err)(15)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{tlsPair},
		}

		s := &http.Server{
			Addr:      *http2Port,
			Handler:   http.FileServer(http.Dir(*webRoot)),
			TLSConfig: tlsConfig,
		}

		http2.ConfigureServer(s, nil)

		go func() {
			err := s.ListenAndServeTLS("", "")
			if err != nil {
				log.Fatal("HTTP2/TLS server error:", err)(2)
			}
		}()
	}
}

func configSetup() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	flag.Usage = func() {
		fmt.Println("WaGo (Watch, Go) build tool. Version", VERSION)
		flag.PrintDefaults()
	}

	// TODO: this should check for actions
	if len(os.Args) < 2 {
		flag.Usage()
		log.Fatal("You must specify an action")(1)
	}

	webBase := flag.String("webbase", "", "Deprecated, use webroot.")
	webServer := flag.String("web", "", "Deprecated, use http or http2.")

	flag.Parse()

	if *verbose {
		log = dog.NewDog(dog.DEBUG)
	} else if *quiet {
		log = dog.NewDog(dog.WARN)
	} else {
		log = dog.NewDog(dog.INFO)
	}

	if *webBase != "" {
		log.Warn("-webbase is deprecated, use -webroot")
		*webRoot = *webBase
	}
	if *webServer != "" {
		log.Warn("-web is deprecated, use -http or -h2")
		*httpPort = *webServer
	}

	if len(*shell) == 0 {
		*shell = os.Getenv("SHELL")
		if len(*shell) == 0 {
			*shell = "/bin/sh"
		}
	}
	log.Debug("Using shell", *shell)

	if (len(*daemonTrigger) > 0) && (*daemonTimer > 0) {
		log.Fatal("Both daemon trigger and timer specified, use only one")(1)
	}

	if (len(*daemonTrigger) > 0 || *daemonTimer > 0) && len(*daemonCmd) == 0 {
		log.Fatal("Specify a daemon command to use the trigger or timer")(1)
	}

	if len(*buildCmd) == 0 && len(*daemonCmd) == 0 && !*fiddle && len(*postCmd) == 0 && len(*url) == 0 && len(*webServer) == 0 && len(*httpPort) == 0 && len(*http2Port) == 0 {
		flag.Usage()
		log.Fatal("You must specify an action")(1)
	}

	if *fiddle {
		if *httpPort == "" {
			*httpPort = ":8420"
		}
		if *http2Port == "" {
			*http2Port = ":8421"
		}
		if *url == "" {
			*url = "http://localhost" + *webServer + "/"
		}
	}

	if *targetDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			panic(err)
		}
		targetDir = &cwd
	}

	if (*keyFile != "" && *certFile == "") || (*certFile != "" && *keyFile == "") {
		log.Fatal("Set both key and cert or none to use default.")(1)
	}

	log.Debug("Target dir:", *targetDir)
}
