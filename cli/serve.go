package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/WatchBeam/clock"
	kitlog "github.com/go-kit/kit/log"
	kitprometheus "github.com/go-kit/kit/metrics/prometheus"
	"github.com/kolide/kolide-ose/server/config"
	"github.com/kolide/kolide-ose/server/datastore/inmem"
	"github.com/kolide/kolide-ose/server/datastore/mysql"
	"github.com/kolide/kolide-ose/server/kolide"
	"github.com/kolide/kolide-ose/server/mail"
	"github.com/kolide/kolide-ose/server/pubsub"
	"github.com/kolide/kolide-ose/server/service"
	"github.com/kolide/kolide-ose/server/version"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
)

type initializer interface {
	// Initialize is used to populate a datastore with
	// preloaded data
	Initialize() error
}

func createServeCmd(configManager config.Manager) *cobra.Command {
	var devMode = false

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Launch the kolide server",
		Long: `
Launch the kolide server

Use kolide serve to run the main HTTPS server. The Kolide server bundles
together all static assets and dependent libraries into a statically linked go
binary (which you're executing right now). Use the options below to customize
the way that the kolide server works.
`,
		Run: func(cmd *cobra.Command, args []string) {
			var (
				httpAddr = flag.String("http.addr", ":8080", "HTTP listen address")
				ctx      = context.Background()
				logger   kitlog.Logger
			)
			flag.Parse()

			config := configManager.LoadConfig()

			logger = kitlog.NewLogfmtLogger(os.Stderr)
			logger = kitlog.NewContext(logger).With("ts", kitlog.DefaultTimestampUTC)

			var ds kolide.Datastore
			var err error
			var mailService kolide.MailService

			if devMode {
				fmt.Println(
					"Dev mode enabled, using in-memory DB.\n",
					"Warning: Changes will not be saved across process restarts. This should NOT be used in production.",
				)

				if ds, err = inmem.New(config); err != nil {
					initFatal(err, "initializing inmem database")
				}
				mailService = mail.NewDevService()
			} else {
				const defaultMaxAttempts = 15

				connString := mysql.GetMysqlConnectionString(config.Mysql)
				ds, err = mysql.New(connString, clock.C, mysql.Logger(logger))

				if err != nil {
					initFatal(err, "initializing datastore")
				}
				mailService = mail.NewService()
			}

			if initializingDS, ok := ds.(initializer); ok {
				if err := initializingDS.Initialize(); err != nil {
					initFatal(err, "loading built in data")
				}
			}

			var resultStore kolide.QueryResultStore
			if devMode {
				resultStore = pubsub.NewInmemQueryResults()
			} else {
				redisPool := pubsub.NewRedisPool(config.Redis.Address, config.Redis.Password)
				resultStore = pubsub.NewRedisQueryResults(redisPool)
			}

			svc, err := service.NewService(ds, resultStore, logger, config, mailService, clock.C)
			if err != nil {
				initFatal(err, "initializing service")
			}

			go func() {
				ticker := time.NewTicker(1 * time.Hour)
				for {
					ds.CleanupDistributedQueryCampaigns(time.Now())
					<-ticker.C
				}
			}()

			fieldKeys := []string{"method", "error"}
			requestCount := kitprometheus.NewCounterFrom(prometheus.CounterOpts{
				Namespace: "api",
				Subsystem: "service",
				Name:      "request_count",
				Help:      "Number of requests received.",
			}, fieldKeys)
			requestLatency := kitprometheus.NewSummaryFrom(prometheus.SummaryOpts{
				Namespace: "api",
				Subsystem: "service",
				Name:      "request_latency_microseconds",
				Help:      "Total duration of requests in microseconds.",
			}, fieldKeys)

			svcLogger := kitlog.NewContext(logger).With("component", "service")
			svc = service.NewLoggingService(svc, svcLogger)
			svc = service.NewMetricsService(svc, requestCount, requestLatency)

			httpLogger := kitlog.NewContext(logger).With("component", "http")

			var apiHandler http.Handler
			{
				apiHandler = service.MakeHandler(ctx, svc, config.Auth.JwtKey, httpLogger)
				// WithSetup will check if first time setup is required
				// By performing the same check inside main, we can make server startups
				// more efficient after the first startup.
				if service.RequireSetup(svc, logger) {
					apiHandler = service.WithSetup(svc, logger, apiHandler)
				}
			}

			healthCheckers := map[string]interface{}{
				"datastore":          ds,
				"query_result_store": resultStore,
			}
			http.Handle("/api/", apiHandler)
			http.Handle("/healthz", healthz(healthCheckers))
			http.Handle("/version", version.Handler())
			http.Handle("/metrics", prometheus.Handler())
			http.Handle("/assets/", service.ServeStaticAssets("/assets/"))
			http.Handle("/", service.ServeFrontend())

			errs := make(chan error, 2)
			go func() {
				if !config.Server.TLS || (devMode && !configManager.IsSet("server.tls")) {
					logger.Log("transport", "http", "address", *httpAddr, "msg", "listening")
					errs <- http.ListenAndServe(*httpAddr, nil)
				} else {
					logger.Log("transport", "https", "address", *httpAddr, "msg", "listening")
					errs <- http.ListenAndServeTLS(
						*httpAddr,
						config.Server.Cert,
						config.Server.Key,
						nil,
					)
				}
			}()
			go func() {
				c := make(chan os.Signal)
				signal.Notify(c, syscall.SIGINT)
				errs <- fmt.Errorf("%s", <-c)
			}()

			logger.Log("terminated", <-errs)
		},
	}

	serveCmd.PersistentFlags().BoolVar(&devMode, "dev", false, "Use dev settings (in-mem DB, etc.)")

	return serveCmd
}

// healthz is an http handler which responds with either
// 200 OK if the server can successfuly communicate with it's backends or
// 500 if any of the backends are reporting an issue.
func healthz(deps map[string]interface{}) http.HandlerFunc {
	type healthChecker interface {
		HealthCheck() error
	}

	return func(w http.ResponseWriter, r *http.Request) {
		errs := make(map[string]string)
		for name, dep := range deps {
			if hc, ok := dep.(healthChecker); ok {
				err := hc.HealthCheck()
				if err != nil {
					errs[name] = err.Error()
				}
			}
		}

		if len(errs) > 0 {
			w.WriteHeader(http.StatusInternalServerError)
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			enc.Encode(map[string]interface{}{
				"errors": errs,
			})
		}
	}
}
