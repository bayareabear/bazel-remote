package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
	"context"

	auth "github.com/abbot/go-http-auth"
	"github.com/buchgr/bazel-remote/cache"
	"github.com/buchgr/bazel-remote/cache/disk"
	"github.com/buchgr/bazel-remote/cache/gcs"

	cachehttp "github.com/buchgr/bazel-remote/cache/http"

	"github.com/buchgr/bazel-remote/config"
	"github.com/buchgr/bazel-remote/server"
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
		cli.Int64Flag{
			Name:   "idle_timeout",
			Value:  0,
			Usage:  "The max server idle time in seconds before bazel-remote exit",
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
				ctx.String("tls_key_file"))
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

		h := server.NewHTTPCache(proxyCache, accessLogger, errorLogger)

		http.HandleFunc("/status", h.StatusPageHandler)
		http.HandleFunc("/", maybeAuth(h.CacheHandler, c.HtpasswdFile, c.Host))
		httpServer := &http.Server{Addr: c.Host+":"+strconv.Itoa(c.Port), Handler: nil}

		if c.IdleTimeout > 0 {
			cache.CacheMutex.Lock()
			cache.LastRequestTime = time.Now()
			cache.CacheMutex.Unlock()
			ticker := time.NewTicker(time.Second)
			go func() {
				for {
					select {
					case <-ticker.C:
						currentTime := time.Now()
						cache.CacheMutex.Lock()
						lastRequestTime := cache.LastRequestTime
						cache.CacheMutex.Unlock()
						if int64(currentTime.Sub(lastRequestTime).Seconds()) > c.IdleTimeout {
							accessLogger.Printf("Shutting down server after idling for more than %d seconds", c.IdleTimeout)
							httpServer.Shutdown(context.Background())
						}
					}
				}
			}()
		}

		if len(c.TLSCertFile) > 0 && len(c.TLSKeyFile) > 0 {
			return httpServer.ListenAndServeTLS(c.TLSCertFile,
				c.TLSKeyFile)
		}
		return httpServer.ListenAndServe()
	}

	serverErr := app.Run(os.Args)
	if serverErr != nil {
		log.Fatal("ListenAndServe: ", serverErr)
	}
}

func maybeAuth(fn http.HandlerFunc, htpasswdFile string, host string) http.HandlerFunc {
	if htpasswdFile != "" {
		secrets := auth.HtpasswdFileProvider(htpasswdFile)
		authenticator := auth.NewBasicAuthenticator(host, secrets)
		return auth.JustCheck(authenticator, fn)
	}
	return fn
}
