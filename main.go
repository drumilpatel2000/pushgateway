// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/route"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	dto "github.com/prometheus/client_model/go"
	promlogflag "github.com/prometheus/common/promlog/flag"

	api_v1 "github.com/prometheus/pushgateway/api/v1"
	"github.com/prometheus/pushgateway/asset"
	"github.com/prometheus/pushgateway/handler"
	"github.com/prometheus/pushgateway/storage"
)

func init() {
	prometheus.MustRegister(version.NewCollector("pushgateway"))
}

func wrap(f http.HandlerFunc, name string) http.HandlerFunc {

	return promhttp.InstrumentHandlerCounter(
		handler.HttpCnt.MustCurryWith(prometheus.Labels{"handler": name}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f(w, r)
		}),
	)
}

// logFunc in an adaptor to plug gokit logging into promhttp.HandlerOpts.
type logFunc func(...interface{}) error

func (lf logFunc) Println(v ...interface{}) {
	lf("msg", fmt.Sprintln(v...))
}

func main() {
	var (
		app = kingpin.New(filepath.Base(os.Args[0]), "The Pushgateway")

		listenAddress       = app.Flag("web.listen-address", "Address to listen on for the web interface, API, and telemetry.").Default(":9091").String()
		metricsPath         = app.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		externalURL         = app.Flag("web.external-url", "The URL under which the Pushgateway is externally reachable.").Default("").URL()
		routePrefix         = app.Flag("web.route-prefix", "Prefix for the internal routes of web endpoints. Defaults to the path of --web.external-url.").Default("").String()
		enableLifeCycle     = app.Flag("web.enable-lifecycle", "Enable shutdown via HTTP request.").Default("false").Bool()
		enableAdminAPI      = app.Flag("web.enable-admin-api", "Enable API endpoints for admin control actions.").Default("false").Bool()
		persistenceFile     = app.Flag("persistence.file", "File to persist metrics. If empty, metrics are only kept in memory.").Default("").String()
		persistenceInterval = app.Flag("persistence.interval", "The minimum interval at which to write out the persistence file.").Default("5m").Duration()
		pushUnchecked       = app.Flag("push.disable-consistency-check", "Do not check consistency of pushed metrics. DANGEROUS.").Default("false").Bool()
		promlogConfig       = promlog.Config{}
	)
	promlogflag.AddFlags(app, &promlogConfig)
	app.Version(version.Print("pushgateway"))
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger := promlog.New(&promlogConfig)

	*routePrefix = computeRoutePrefix(*routePrefix, *externalURL)
	externalPathPrefix := computeRoutePrefix("", *externalURL)

	level.Info(logger).Log("msg", "starting pushgateway", "version", version.Info())
	level.Info(logger).Log("build_context", version.BuildContext())
	level.Debug(logger).Log("msg", "external URL", "url", *externalURL)
	level.Debug(logger).Log("msg", "path prefix used externally", "path", externalPathPrefix)
	level.Debug(logger).Log("msg", "path prefix for internal routing", "path", *routePrefix)

	// flags is used to show command line flags on the status page.
	// Kingpin default flags are excluded as they would be confusing.
	flags := map[string]string{}
	boilerplateFlags := kingpin.New("", "").Version("")
	for _, f := range app.Model().Flags {
		if boilerplateFlags.GetFlag(f.Name) == nil {
			flags[f.Name] = f.Value.String()
		}
	}

	ms := storage.NewDiskMetricStore(*persistenceFile, *persistenceInterval, prometheus.DefaultGatherer, logger)

	// Create a Gatherer combining the DefaultGatherer and the metrics from the metric store.
	g := prometheus.Gatherers{
		prometheus.DefaultGatherer,
		prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) { return ms.GetMetricFamilies(), nil }),
	}

	r := route.New()
	r.Get(*routePrefix+"/-/healthy", wrap(handler.Healthy(ms).ServeHTTP, "healthy"))
	r.Get(*routePrefix+"/-/ready", wrap(handler.Ready(ms).ServeHTTP, "ready"))
	r.Get(
		path.Join(*routePrefix, *metricsPath),
		promhttp.HandlerFor(g, promhttp.HandlerOpts{
			ErrorLog: logFunc(level.Error(logger).Log),
		}).ServeHTTP,
	)

	if *enableAdminAPI {
		// To be consistent with Prometheus codebase and provide endpoint versioning, we use the same path
		// as Prometheus for its admin endpoints, even if this may feel excesive for just one simple endpoint
		// this will likely change over time.
		r.Put(*routePrefix+"/api/v1/admin/wipe", wrap(handler.WipeMetricStore(ms, logger).ServeHTTP, "wipe"))
	}

	// Handlers for pushing and deleting metrics.
	pushAPIPath := *routePrefix + "/metrics"
	for _, suffix := range []string{"", handler.Base64Suffix} {
		jobBase64Encoded := suffix == handler.Base64Suffix
		r.Put(pushAPIPath+"/job"+suffix+"/:job/*labels", wrap(handler.Push(ms, true, !*pushUnchecked, jobBase64Encoded, logger), "push"))
		r.Post(pushAPIPath+"/job"+suffix+"/:job/*labels", wrap(handler.Push(ms, false, !*pushUnchecked, jobBase64Encoded, logger), "push"))
		r.Del(pushAPIPath+"/job"+suffix+"/:job/*labels", wrap(handler.Delete(ms, jobBase64Encoded, logger), "delete"))
		r.Put(pushAPIPath+"/job"+suffix+"/:job", wrap(handler.Push(ms, true, !*pushUnchecked, jobBase64Encoded, logger), "push"))
		r.Post(pushAPIPath+"/job"+suffix+"/:job", wrap(handler.Push(ms, false, !*pushUnchecked, jobBase64Encoded, logger), "push"))
		r.Del(pushAPIPath+"/job"+suffix+"/:job", wrap(handler.Delete(ms, jobBase64Encoded, logger), "delete"))
	}
	r.Get(*routePrefix+"/static/*filepath", wrap(handler.Static(asset.Assets, *routePrefix).ServeHTTP, "static"))

	statusHandler := handler.Status(ms, asset.Assets, flags, externalPathPrefix, logger)
	r.Get(*routePrefix+"/status", wrap(statusHandler.ServeHTTP, "status"))
	r.Get(*routePrefix+"/", wrap(statusHandler.ServeHTTP, "status"))

	// Re-enable pprof.
	r.Get(*routePrefix+"/debug/pprof/*pprof", handlePprof)

	level.Info(logger).Log("listen_address", *listenAddress)
	l, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		level.Error(logger).Log("err", err)
		os.Exit(1)
	}

	quitCh := make(chan struct{})
	quitHandler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Requesting termination... Goodbye!")
		close(quitCh)
	}

	forbiddenAPINotEnabled := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("Lifecycle API is not enabled."))
	}

	if *enableLifeCycle {
		r.Put(*routePrefix+"/-/quit", quitHandler)
		r.Post(*routePrefix+"/-/quit", quitHandler)
	} else {
		r.Put(*routePrefix+"/-/quit", forbiddenAPINotEnabled)
		r.Post(*routePrefix+"/-/quit", forbiddenAPINotEnabled)
	}

	r.Get("/-/quit", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("Only POST or PUT requests allowed."))
	})

	mux := http.NewServeMux()
	mux.Handle("/", r)

	buildInfo := map[string]string{
		"version":   version.Version,
		"revision":  version.Revision,
		"branch":    version.Branch,
		"buildUser": version.BuildUser,
		"buildDate": version.BuildDate,
		"goVersion": version.GoVersion,
	}

	apiv1 := api_v1.New(logger, ms, flags, buildInfo)

	apiPath := "/api"
	if *routePrefix != "/" {
		apiPath = *routePrefix + apiPath
	}

	av1 := route.New()
	apiv1.Register(av1)

	mux.Handle(apiPath+"/v1/", http.StripPrefix(apiPath+"/v1", av1))

	go closeListenerOnQuit(l, quitCh, logger)
	err = (&http.Server{Addr: *listenAddress, Handler: mux}).Serve(l)
	level.Error(logger).Log("msg", "HTTP server stopped", "err", err)
	// To give running connections a chance to submit their payload, we wait
	// for 1sec, but we don't want to wait long (e.g. until all connections
	// are done) to not delay the shutdown.
	time.Sleep(time.Second)
	if err := ms.Shutdown(); err != nil {
		level.Error(logger).Log("msg", "problem shutting down metric storage", "err", err)
	}
}

func handlePprof(w http.ResponseWriter, r *http.Request) {
	switch route.Param(r.Context(), "pprof") {
	case "/cmdline":
		pprof.Cmdline(w, r)
	case "/profile":
		pprof.Profile(w, r)
	case "/symbol":
		pprof.Symbol(w, r)
	default:
		pprof.Index(w, r)
	}
}

// computeRoutePrefix returns the effective route prefix based on the
// provided flag values for --web.route-prefix and
// --web.external-url. With prefix empty, the path of externalURL is
// used instead. A prefix "/" results in an empty returned prefix. Any
// non-empty prefix is normalized to start, but not to end, with "/".
func computeRoutePrefix(prefix string, externalURL *url.URL) string {
	if prefix == "" {
		prefix = externalURL.Path
	}

	if prefix == "/" {
		prefix = ""
	}

	if prefix != "" {
		prefix = "/" + strings.Trim(prefix, "/")
	}

	return prefix
}

// closeListenerOnQuite closes the provided listener upon closing the provided
// quitCh or upon receiving a SIGINT or SIGTERM.
func closeListenerOnQuit(l net.Listener, quitCh <-chan struct{}, logger log.Logger) {
	notifier := make(chan os.Signal, 1)
	signal.Notify(notifier, os.Interrupt, syscall.SIGTERM)

	select {
	case <-notifier:
		level.Info(logger).Log("msg", "received SIGINT/SIGTERM; exiting gracefully...")
		break
	case <-quitCh:
		level.Warn(logger).Log("msg", "received termination request via web service, exiting gracefully...")
		break
	}
	l.Close()
}
