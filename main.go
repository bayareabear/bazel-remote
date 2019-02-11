package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
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

const (
	bazelRemotePidFile     = "bazel-remote.pid"
	bazelRemotePidFileLock = "bazel-remote.pid.lock"
	tryLockAttempt         = 3
)

//http server.go doesn't export tcpKeepAliveListener so we have to do the same here
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

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
		wrapGracefulShutdown(accessLogger, httpServer, c.Dir)

		if c.IdleTimeout > 0 {
			cacheHandler = wrapIdleHandler(cacheHandler, c.IdleTimeout, accessLogger, httpServer, c.Dir)
		}

		mux.HandleFunc("/", cacheHandler)
		ln, err := net.Listen("tcp", c.Host+":"+strconv.Itoa(c.Port))
		if err != nil {
			return err
		}
		defer ln.Close()
		err = writePidFile(c.Dir, c.Port)
		if err != nil {
			return err
		}
		if len(c.TLSCertFile) > 0 && len(c.TLSKeyFile) > 0 {
			return httpServer.ServeTLS(tcpKeepAliveListener{ln.(*net.TCPListener)}, c.TLSCertFile, c.TLSKeyFile)
		}
		return httpServer.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
	}
	serverErr := app.Run(os.Args)
	if serverErr != nil {
		log.Fatal("bazel-remote terminated: ", serverErr)
	}
}

func wrapIdleHandler(handler http.HandlerFunc, idleTimeout time.Duration, accessLogger cache.Logger, httpServer *http.Server, cacheDirectory string) http.HandlerFunc {
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
					removePidFile(cacheDirectory)
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

func wrapGracefulShutdown(accessLogger cache.Logger, httpServer *http.Server, cacheDirectory string) {
	signalReceiver := make(chan os.Signal, 1)
	signal.Notify(signalReceiver, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-signalReceiver:
			accessLogger.Printf("Gracefully shutting down server due to %v signal", sig)
			removePidFile(cacheDirectory)
			httpServer.Shutdown(context.Background())
		}
	}()
}

func wrapAuthHandler(handler http.HandlerFunc, htpasswdFile string, host string) http.HandlerFunc {
	secrets := auth.HtpasswdFileProvider(htpasswdFile)
	authenticator := auth.NewBasicAuthenticator(host, secrets)
	return auth.JustCheck(authenticator, handler)
}

func writePidFile(cachDirectory string, port int) error {
	// create a bazel-remote.pid file for recording pid information
	// Lock bazel-remote.pid
	err := os.MkdirAll(cachDirectory, os.FileMode(0755))
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(cachDirectory)
	if err != nil {
		return err
	}
	pidFileLock, err := tryLockFile(filepath.Join(absolutePath, bazelRemotePidFileLock), tryLockAttempt)
	if err != nil {
		return err
	}
	defer pidFileLock.Unlock()

	// Check if there is an existing process running
	pidFile := filepath.Join(cachDirectory, bazelRemotePidFile)
	fileContent, err := ioutil.ReadFile(pidFile)
	if err == nil && len(fileContent) > 0 {
		words := strings.Split(string(fileContent), " ")
		pid, err := strconv.Atoi(words[0])
		if err == nil {
			bazelRemoteProcess, err := os.FindProcess(pid)
			err = bazelRemoteProcess.Signal(syscall.Signal(0))
			if err == nil {
				return fmt.Errorf("a bazel-remote process %v is already running with port %v", words[0], words[1])
			}
		}
	}

	// Write pid information into bazel-remote.pid file under cache directory
	currentPid := []byte(strconv.Itoa(os.Getpid()) + " " + strconv.Itoa(port))
	err = ioutil.WriteFile(pidFile, currentPid, 0755)
	return err
}

func removePidFile(cacheDirectory string) error {
	// Lock bazel-remote.pid
	absolutePath, err := filepath.Abs(cacheDirectory)
	if err != nil {
		return err
	}
	pidFileLock, err := tryLockFile(filepath.Join(absolutePath, bazelRemotePidFileLock), tryLockAttempt)
	if err != nil {
		return err
	}
	defer pidFileLock.Unlock()

	// Delete bazel-remote.pid
	pidFile := filepath.Join(cacheDirectory, bazelRemotePidFile)
	return os.Remove(pidFile)
}

func tryLockFile(filePath string, lockAttempt int) (lockfile.Lockfile, error) {
	fileLock, err := lockfile.New(filePath)
	if err != nil {
		return lockfile.Lockfile(""), fmt.Errorf("cannot init lock: %v", err)
	}
	err = fileLock.TryLock()
	for err != nil && lockAttempt > 0 {
		err = fileLock.TryLock()
		time.Sleep(time.Second)
		lockAttempt--
	}
	if err != nil {
		return lockfile.Lockfile(""), fmt.Errorf("could not lock %q: %v", fileLock, err)
	}
	return fileLock, nil
}
