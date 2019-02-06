package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	auth "github.com/abbot/go-http-auth"
	"github.com/buchgr/bazel-remote/cache"
	"github.com/buchgr/bazel-remote/cache/disk"
	"github.com/buchgr/bazel-remote/cache/gcs"

	cachehttp "github.com/buchgr/bazel-remote/cache/http"

	"github.com/buchgr/bazel-remote/config"
	"github.com/buchgr/bazel-remote/server"
	"github.com/nightlyone/lockfile"
	"github.com/urfave/cli"
)

func main() {
	app := cli.NewApp()
	app.Description = "A remote build cache for Bazel."
	app.Usage = "A remote build cache for Bazel"
	app.HideVersion = true

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "config_file",
			Value: "",
			Usage: "Path to a YAML configuration file. If this flag is specified then all other flags " +
				"are ignored.",
			EnvVar: "BAZEL_REMOTE_CONFIG_FILE",
		},
		cli.StringFlag{
			Name:   "dir",
			Value:  "",
			Usage:  "Directory path where to store the cache contents. This flag is required.",
			EnvVar: "BAZEL_REMOTE_DIR",
		},
		cli.Int64Flag{
			Name:   "max_size",
			Value:  -1,
			Usage:  "The maximum size of the remote cache in GiB. This flag is required.",
			EnvVar: "BAZEL_REMOTE_MAX_SIZE",
		},
		cli.StringFlag{
			Name:   "host",
			Value:  "",
			Usage:  "Address to listen on. Listens on all network interfaces by default.",
			EnvVar: "BAZEL_REMOTE_HOST",
		},
		cli.IntFlag{
			Name:   "port",
			Value:  8080,
			Usage:  "The port the HTTP server listens on.",
			EnvVar: "BAZEL_REMOTE_PORT",
		},
		cli.StringFlag{
			Name:   "htpasswd_file",
			Value:  "",
			Usage:  "Path to a .htpasswd file. This flag is optional. Please read https://httpd.apache.org/docs/2.4/programs/htpasswd.html.",
			EnvVar: "BAZEL_REMOTE_HTPASSWD_FILE",
		},
		cli.BoolFlag{
			Name:   "tls_enabled",
			Usage:  "This flag has been deprecated. Specify tls_cert_file and tls_key_file instead.",
			EnvVar: "BAZEL_REMOTE_TLS_ENABLED",
		},
		cli.StringFlag{
			Name:   "tls_cert_file",
			Value:  "",
			Usage:  "Path to a pem encoded certificate file.",
			EnvVar: "BAZEL_REMOTE_TLS_CERT_FILE",
		},
		cli.StringFlag{
			Name:   "tls_key_file",
			Value:  "",
			Usage:  "Path to a pem encoded key file.",
			EnvVar: "BAZEL_REMOTE_TLS_KEY_FILE",
		},
		cli.DurationFlag{
			Name:   "idle_timeout",
			Value:  0,
			Usage:  "The maximum period of having received no request after which the server will shut itself down. Disabled by default.",
			EnvVar: "BAZEL_REMOTE_IDLE_TIMEOUT",
		},
	}

	app.Action = func(ctx *cli.Context) error {
		configFile := ctx.String("config_file")
		var c *config.Config
		var err error
		if configFile != "" {
			c, err = config.NewFromYamlFile(configFile)
		} else {
			c, err = config.New(ctx.String("dir"),
				ctx.Int("max_size"),
				ctx.String("host"),
				ctx.Int("port"),
				ctx.String("htpasswd_file"),
				ctx.String("tls_cert_file"),
				ctx.String("tls_key_file"),
				ctx.Duration("idle_timeout"))
		}

		if err != nil {
			fmt.Fprintf(ctx.App.Writer, "%v\n\n", err)
			cli.ShowAppHelp(ctx)
			return nil
		}

		accessLogger := log.New(os.Stdout, "", log.Ldate|log.Ltime|log.LUTC)
		errorLogger := log.New(os.Stderr, "", log.Ldate|log.Ltime|log.LUTC)

		if err = writePidFileDuringStartup(c, accessLogger); err != nil {
			return err
		}

		diskCache := disk.New(c.Dir, int64(c.MaxSize)*1024*1024*1024)

		var proxyCache cache.Cache
		if c.GoogleCloudStorage != nil {
			proxyCache, err = gcs.New(c.GoogleCloudStorage.Bucket,
				c.GoogleCloudStorage.UseDefaultCredentials, c.GoogleCloudStorage.JSONCredentialsFile,
				diskCache, accessLogger, errorLogger)
			if err != nil {
				log.Fatal(err)
			}
		} else if c.HTTPBackend != nil {
			httpClient := &http.Client{}
			baseURL, err := url.Parse(c.HTTPBackend.BaseURL)
			if err != nil {
				log.Fatal(err)
			}
			proxyCache = cachehttp.New(baseURL, diskCache,
				httpClient, accessLogger, errorLogger)
		} else {
			proxyCache = diskCache
		}

		mux := http.NewServeMux()
		httpServer := &http.Server{
			Addr:    c.Host + ":" + strconv.Itoa(c.Port),
			Handler: mux,
		}
		h := server.NewHTTPCache(proxyCache, accessLogger, errorLogger)
		mux.HandleFunc("/status", h.StatusPageHandler)

		cacheHandler := h.CacheHandler
		if c.HtpasswdFile != "" {
			cacheHandler = wrapAuthHandler(cacheHandler, c.HtpasswdFile, c.Host)
		}

		// Gracefully shutdown when terminate with ctrl + c
		cacheHandler = wrapGraceShutdownHandler(cacheHandler, accessLogger, httpServer, c)

		if c.IdleTimeout > 0 {
			cacheHandler = wrapIdleHandler(cacheHandler, c.IdleTimeout, accessLogger, httpServer, c)
		}

		mux.HandleFunc("/", cacheHandler)

		if len(c.TLSCertFile) > 0 && len(c.TLSKeyFile) > 0 {
			return httpServer.ListenAndServeTLS(c.TLSCertFile, c.TLSKeyFile)
		}
		return httpServer.ListenAndServe()
	}

	serverErr := app.Run(os.Args)
	if serverErr != nil {
		log.Fatal("bazel-remote terminated: ", serverErr)
	}
}

func wrapIdleHandler(handler http.HandlerFunc, idleTimeout time.Duration, accessLogger cache.Logger, httpServer *http.Server, c *config.Config) http.HandlerFunc {
	lastRequest := time.Now()
	ticker := time.NewTicker(time.Second)
	var m sync.Mutex
	go func() {
		for {
			select {
			case now := <-ticker.C:
				m.Lock()
				elapsed := now.Sub(lastRequest)
				m.Unlock()
				if elapsed > idleTimeout {
					ticker.Stop()
					accessLogger.Printf("Shutting down server after having been idle for %v", idleTimeout)
					removePidInfoBeforeShutDown(accessLogger, c)
					httpServer.Shutdown(context.Background())
				}
			}
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		m.Lock()
		lastRequest = now
		m.Unlock()
		handler(w, r)
	})
}

func wrapGraceShutdownHandler(handler http.HandlerFunc, accessLogger cache.Logger, httpServer *http.Server, c *config.Config) http.HandlerFunc {
	signalReceiver := make(chan os.Signal, 1)
	signal.Notify(signalReceiver, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-signalReceiver:
			accessLogger.Printf("Gracefully shutting down server due to %v signal", sig)
			removePidInfoBeforeShutDown(accessLogger, c)
			httpServer.Shutdown(context.Background())
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r)
	})
}

func wrapAuthHandler(handler http.HandlerFunc, htpasswdFile string, host string) http.HandlerFunc {
	secrets := auth.HtpasswdFileProvider(htpasswdFile)
	authenticator := auth.NewBasicAuthenticator(host, secrets)
	return auth.JustCheck(authenticator, handler)
}

func writePidFileDuringStartup(c *config.Config, accessLogger cache.Logger) error {
	// create a bazel-remote.pid file for recording pid information
	// Lock bazel-remote.pid
	err := os.MkdirAll(c.Dir, os.FileMode(0755))
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(c.Dir)
	if err != nil {
		return err
	}
	pidFileLock, err := lockfile.New(filepath.Join(absolutePath, "bazel-remote.pid.lock"))
	if err != nil {
		return fmt.Errorf("Init file lock failed: %v", err)
	}
	attempt := 3
	for err != nil && attempt > 0 {
		err = pidFileLock.TryLock()
		time.Sleep(time.Second)
		attempt--
	}

	if err != nil {
		return fmt.Errorf("Lock %q failed: %v", pidFileLock, err)
	}
	defer pidFileLock.Unlock()

	// Check if there is an existing process running
	pidFile := filepath.Join(c.Dir, "bazel-remote.pid")
	fileContent, err := ioutil.ReadFile(pidFile)
	if err == nil && len(fileContent) > 0 {
		words := strings.Split(string(fileContent), " ")
		pid, err := strconv.Atoi(words[1])
		if err == nil {
			bazelRemoteProcess, err := os.FindProcess(pid)
			if err = bazelRemoteProcess.Signal(syscall.Signal(0)); err == nil {
				return fmt.Errorf("A bazel-remote process is already running with %v", string(fileContent))
			}
		}
	}

	// Write pid information into bazel-remote.pid file under cache directory
	currentPid := []byte(
		"pid:" + " " + strconv.Itoa(os.Getpid()) + " " +
			"port:" + " " + strconv.Itoa(c.Port) + "\n")
	err = ioutil.WriteFile(pidFile, currentPid, 0755)
	return err
}

func removePidInfoBeforeShutDown(accessLogger cache.Logger, c *config.Config) error {
	// Lock bazel-remote.pid
	absolutePath, err := filepath.Abs(c.Dir)
	if err != nil {
		return err
	}
	pidFileLock, err := lockfile.New(filepath.Join(absolutePath, "bazel-remote.pid.lock"))
	if err != nil {
		return fmt.Errorf("Init file lock failed: %v", err)
	}
	attempt := 3
	for err != nil && attempt > 0 {
		err = pidFileLock.TryLock()
		time.Sleep(time.Second)
		attempt--
	}
	if err != nil {
		return fmt.Errorf("Lock %q failed: %v", pidFileLock, err)
	}
	defer pidFileLock.Unlock()

	// Delete bazel-remote.pid
	pidFile := filepath.Join(c.Dir, "bazel-remote.pid")
	return os.Remove(pidFile)
}
