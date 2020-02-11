// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The fetch command runs a server that fetches modules from a proxy and writes
// them to the discovery database.
package main

import (
	"bufio"
	"context"
	"flag"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/errorreporting"
	"cloud.google.com/go/profiler"
	"github.com/go-redis/redis/v7"
	"golang.org/x/discovery/internal/config"
	"golang.org/x/discovery/internal/database"
	"golang.org/x/discovery/internal/dcensus"
	"golang.org/x/discovery/internal/etl"
	"golang.org/x/discovery/internal/index"

	"golang.org/x/discovery/internal/log"
	"golang.org/x/discovery/internal/middleware"
	"golang.org/x/discovery/internal/postgres"
	"golang.org/x/discovery/internal/proxy"

	"contrib.go.opencensus.io/integrations/ocsql"
)

var (
	timeout    = config.GetEnv("GO_DISCOVERY_ETL_TIMEOUT_MINUTES", "10")
	queueName  = config.GetEnv("GO_DISCOVERY_ETL_TASK_QUEUE", "dev-fetch-tasks")
	workers    = flag.Int("workers", 10, "number of concurrent requests to the fetch service, when running locally")
	staticPath = flag.String("static", "content/static", "path to folder containing static files served")
)

func main() {
	flag.Parse()

	ctx := context.Background()

	cfg, err := config.Init(ctx)
	if err != nil {
		log.Fatal(ctx, err)
	}
	cfg.Dump(os.Stderr)

	if cfg.UseProfiler {
		if err := profiler.Start(profiler.Config{}); err != nil {
			log.Fatalf(ctx, "profiler.Start: %v", err)
		}
	}

	readProxyRemoved(ctx)

	// Wrap the postgres driver with OpenCensus instrumentation.
	driverName, err := ocsql.Register("postgres", ocsql.WithAllTraceOptions())
	if err != nil {
		log.Fatalf(ctx, "unable to register the ocsql driver: %v\n", err)
	}
	ddb, err := database.Open(driverName, cfg.DBConnInfo())
	if err != nil {
		log.Fatalf(ctx, "database.Open: %v", err)
	}
	db := postgres.New(ddb)
	defer db.Close()

	populateExcluded(ctx, db)

	indexClient, err := index.New(cfg.IndexURL)
	if err != nil {
		log.Fatal(ctx, err)
	}
	proxyClient, err := proxy.New(cfg.ProxyURL)
	if err != nil {
		log.Fatal(ctx, err)
	}
	fetchQueue := queue(ctx, proxyClient, db)
	reportingClient := reportingClient(ctx)
	redisClient := getRedis(ctx, cfg)
	server, err := etl.NewServer(db, indexClient, proxyClient, redisClient, fetchQueue, reportingClient, *staticPath)
	if err != nil {
		log.Fatal(ctx, err)
	}
	router := dcensus.NewRouter(nil)
	server.Install(router.Handle)

	views := append(dcensus.ClientViews, dcensus.ServerViews...)
	if err := dcensus.Init(views...); err != nil {
		log.Fatal(ctx, err)
	}
	// We are not currently forwarding any ports on AppEngine, so serving debug
	// information is broken.
	if !config.OnAppEngine() {
		dcensusServer, err := dcensus.NewServer()
		if err != nil {
			log.Fatal(ctx, err)
		}
		go http.ListenAndServe(config.DebugAddr("localhost:8001"), dcensusServer)
	}

	handlerTimeout, err := strconv.Atoi(timeout)
	if err != nil {
		log.Fatalf(ctx, "strconv.Atoi(%q): %v", timeout, err)
	}
	requestLogger := logger(ctx)
	mw := middleware.Chain(
		middleware.RequestLog(requestLogger),
		middleware.Timeout(time.Duration(handlerTimeout)*time.Minute),
	)
	http.Handle("/", mw(router))

	addr := config.HostAddr("localhost:8000")
	log.Infof(ctx, "Listening on addr %s", addr)
	log.Fatal(ctx, http.ListenAndServe(addr, nil))
}

func queue(ctx context.Context, proxyClient *proxy.Client, db *postgres.DB) etl.Queue {
	if !config.OnAppEngine() {
		return etl.NewInMemoryQueue(ctx, proxyClient, db, *workers)
	}
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		log.Fatal(ctx, err)
	}
	return etl.NewGCPQueue(client, queueName)
}

func getRedis(ctx context.Context, cfg *config.Config) *redis.Client {
	if cfg.RedisHAHost != "" {
		return redis.NewClient(&redis.Options{
			Addr: cfg.RedisHAHost + ":" + cfg.RedisHAPort,
			// We update completions with one big pipeline, so we need long write
			// timeouts. ReadTimeout is increased only to be consistent with
			// WriteTimeout.
			WriteTimeout: 5 * time.Minute,
			ReadTimeout:  5 * time.Minute,
		})
	}
	return nil
}

func reportingClient(ctx context.Context) *errorreporting.Client {
	if !config.OnAppEngine() {
		return nil
	}
	reporter, err := errorreporting.NewClient(ctx, config.ProjectID(), errorreporting.Config{
		ServiceName: config.ServiceID(),
		OnError: func(err error) {
			log.Errorf(ctx, "Error reporting failed: %v", err)
		},
	})
	if err != nil {
		log.Fatal(ctx, err)
	}
	return reporter
}

func logger(ctx context.Context) middleware.Logger {
	if config.OnAppEngine() {
		logger, err := log.UseStackdriver(ctx, "etl-log")
		if err != nil {
			log.Fatal(ctx, err)
		}
		return logger
	}
	return middleware.LocalLogger{}
}

// Read a file of module versions that we should ignore because
// the are in the index but not stored in the proxy.
// Format of the file: each line is
//     module@version
func readProxyRemoved(ctx context.Context) {
	filename := config.GetEnv("GO_DISCOVERY_PROXY_REMOVED", "")
	if filename == "" {
		return
	}
	f, err := os.Open(filename)
	if err != nil {
		log.Fatal(ctx, err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		etl.ProxyRemoved[strings.TrimSpace(scan.Text())] = true
	}
	if err := scan.Err(); err != nil {
		log.Fatalf(ctx, "scanning %s: %v", filename, err)
	}
	log.Infof(ctx, "read %d excluded module versions from %s", len(etl.ProxyRemoved), filename)
}

// excludedPrefixes is a list of excluded prefixes and the reasons for exclusion.
// This is a permanent record of exclusions, in case the DB gets wiped or corrupted.
var excludedPrefixes = []struct {
	prefix, reason string
}{
	{
		"github.com/xvrzhao/site-monitor",
		"author requested https://groups.google.com/a/google.com/d/msg/go-discovery-feedback/oYtPw2Ob0fY/xxGikZK1AQAJ",
	},
	{
		"gioui.org/ui",
		"author requested https://groups.google.com/a/google.com/d/msg/go-discovery-feedback/CeMEn2E1zwo/q5S8HPn6BgAJ",
	},
	{
		"github.com/kortschak/unlicensable",
		"https://groups.google.com/g/golang-dev/c/mfiPCtJ1BGU/m/HDb3--vMEwAJk",
	},
	{
		"github.com/clevergo/clevergo",
		"https://groups.google.com/a/google.com/g/go-discovery-feedback/c/IAHYXlstv-g/m/muE06-ECFgAJ",
	},
}

// populateExcluded adds each element of excludedPrefixes to the excluded_prefixes
// table if it isn't already present.
func populateExcluded(ctx context.Context, db *postgres.DB) {
	user := os.Getenv("USER")
	if user == "" {
		user = "etl"
	}
	for _, ep := range excludedPrefixes {
		present, err := db.IsExcluded(ctx, ep.prefix)
		if err != nil {
			log.Fatalf(ctx, "db.IsExcluded(%q): %v", ep.prefix, err)
		}
		if !present {
			if err := db.InsertExcludedPrefix(ctx, ep.prefix, user, ep.reason); err != nil {
				log.Fatalf(ctx, "db.InsertExcludedPrefix(%q, %q, %q): %v", ep.prefix, user, ep.reason, err)
			}
		}
	}
}
