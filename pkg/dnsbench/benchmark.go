package dnsbench

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/schollz/progressbar/v3"
	"github.com/tantalor93/dnspyre/v3/pkg/printutils"
	"go.uber.org/ratelimit"
)

var client = http.Client{
	Timeout: 120 * time.Second,
}

const (
	// UDPTransport represents plain DNS over UDP.
	UDPTransport = "udp"
	// TCPTransport represents plain DNS over TCP.
	TCPTransport = "tcp"
	// TLSTransport represents DNS over TLS.
	TLSTransport = "tcp-tls"
	// QUICTransport represents DNS over QUIC.
	QUICTransport = "quic"

	// GetHTTPMethod represents GET HTTP Method for DoH.
	GetHTTPMethod = "get"
	// PostHTTPMethod represents GET POST Method for DoH.
	PostHTTPMethod = "post"

	// HTTP1Proto represents HTTP/1.1 protocol for DoH.
	HTTP1Proto = "1.1"
	// HTTP2Proto represents HTTP/2 protocol for DoH.
	HTTP2Proto = "2"
	// HTTP3Proto represents HTTP/3 protocol for DoH.
	HTTP3Proto = "3"

	// DefaultEdns0BufferSize default EDNS0 buffer size according to the http://www.dnsflagday.net/2020/
	DefaultEdns0BufferSize = 1232

	// DefaultRequestLogPath is a default path to the file, where the requests will be logged.
	DefaultRequestLogPath = "requests.log"

	// DefaultPlotFormat is a default format for plots.
	DefaultPlotFormat = "svg"

	// DefaultRequestTimeout is a default request timeout.
	DefaultRequestTimeout = 5 * time.Second

	// DefaultConnectTimeout is a default connect timeout.
	DefaultConnectTimeout = time.Second

	// DefaultReadTimeout is a default read timeout.
	DefaultReadTimeout = 3 * time.Second

	// DefaultWriteTimeout is a default read timeout.
	DefaultWriteTimeout = time.Second
)

// Benchmark is representation of runnable DNS benchmark scenario.
// based on domains provided in Benchmark.Queries, it will be firing DNS queries until
// the desired number of queries have been sent by each concurrent worker (see Benchmark.Count) or the desired
// benchmark duration have been reached (see Benchmark.Duration).
//
// Benchmark will create Benchmark.Concurrency worker goroutines, where each goroutine will be generating DNS queries
// with domains defined using Benchmark.Queries and DNS query types defined in Benchmark.Types. Each worker will
// either generate Benchmark.Types*Benchmark.Count*len(Benchmark.Queries) number of queries if Benchmark.Count is specified,
// or the worker will be generating arbitrary number of queries until Benchmark.Duration is reached.
type Benchmark struct {
	// Server represents (plain DNS, DoT, DoH or DoQ) server, which will be benchmarked.
	// Format depends on the DNS protocol, that should be used for DNS benchmark.
	// For plain DNS (either over UDP or TCP) the format is <IP/host>[:port], if port is not provided then port 53 is used.
	// For DoT the format is <IP/host>[:port], if port is not provided then port 853 is used.
	// For DoH the format is https://<IP/host>[:port][/path] or http://<IP/host>[:port][/path], if port is not provided then either 443 or 80 port is used. If no path is provided, then /dns-query is used.
	// For DoQ the format is quic://<IP/host>[:port], if port is not provided then port 853 is used.
	Server string

	// Types is an array of DNS query types, that should be used in benchmark. All domains retrieved from domain data source will be fired with each
	// type specified here.
	Types []string

	// Count specifies how many times each domain from data source is used by each worker. Either Benchmark.Count or Benchmark.Duration must be specified.
	// If Benchmark.Count and Benchmark.Duration is specified at once, it is considered invalid state of Benchmark.
	Count int64

	// Duration specifies for how long the benchmark should be executing, the benchmark will run for the specified time
	// while sending DNS requests in an infinite loop based on the data source. After running for the specified duration, the benchmark is canceled.
	// This option is exclusive with Benchmark.Count.
	Duration time.Duration

	// Concurrency controls how many concurrent queries will be issued at once. Benchmark will spawn Concurrency number of parallel worker goroutines.
	Concurrency uint32

	// Rate configures global rate limit for queries per second. This limit is shared between all the worker goroutines. This means that queries generated by this Benchmark
	// per second will not exceed this limit.
	Rate int
	// RateLimitWorker configures rate limit per worker for queries per second. This means that queries generated by each concurrent worker per second will not exceed this limit.
	RateLimitWorker int

	// QperConn configures how many queries are sent by each connection (socket) before closing it and creating a new one.
	// This is considered only for plain DNS over UDP or TCP and DoT.
	QperConn int64

	// Recurse configures whether the DNS queries generated by this Benchmark have Recursion Desired (RD) flag set.
	Recurse bool

	// Probability is used to bring randomization into Benchmark runs. When Probability is 1 or above, then all the domains passed in Queries field will be used during Benchmark run.
	// When Probability is less than 1 and more than 0, then each domain in Queries has Probability chance to be used during benchmark.
	// When Probability is less than 0, then no domain from Queries is used during benchmark.
	Probability float64

	// EdnsOpt specifies EDNS option with code point code and optionally payload of value as a hexadecimal string in format code[:value].
	// code must be an arbitrary numeric value.
	EdnsOpt string

	// DNSSEC Allow DNSSEC (sets DO bit for all DNS requests to 1)
	DNSSEC bool

	// Edns0 configures EDNS0 usage in DNS requests send by benchmark and configures EDNS0 buffer size to the specified value. When 0 is configured, then EDNS0 is not used.
	Edns0 uint16

	// TCP controls whether plain DNS benchmark uses TCP or UDP. When true, the TCP is used.
	TCP bool

	// DOT controls whether DoT is used for the benchmark.
	DOT bool

	// WriteTimeout configures write timeout for DNS requests generated by Benchmark.
	WriteTimeout time.Duration
	// ReadTimeout configures read timeout for DNS responses.
	ReadTimeout time.Duration
	// ConnectTimeout configures timeout for connection establishment.
	ConnectTimeout time.Duration
	// RequestTimeout configures overall timeout for a single DNS request.
	RequestTimeout time.Duration

	// Rcodes controls whether ResultStats.Codes is filled in Benchmark results.
	Rcodes bool

	// HistDisplay controls whether Benchmark.PrintReport will include histogram.
	HistDisplay bool
	// HistMin controls minimum value of histogram printed by Benchmark.PrintReport.
	HistMin time.Duration
	// HistMax controls maximum value of histogram printed by Benchmark.PrintReport.
	HistMax time.Duration
	// HistPre controls precision of histogram printed by Benchmark.PrintReport.
	HistPre int

	// Csv path to file, where the Benchmark result distribution is written.
	Csv string
	// JSON controls whether the Benchmark.PrintReport prints the Benchmark results in JSON format (option is true).
	JSON bool

	// Silent controls whether the Benchmark.Run and Benchmark.PrintReport writes anything to stdout.
	Silent bool
	// Color controls coloring of std output.
	Color bool

	// PlotDir controls where the generated graphs are exported. If set to empty (""), which is default value. Then no graphs are generated.
	PlotDir string
	// PlotFormat controls the format of generated graphs. Supported values are "svg", "png" and "jpg".
	PlotFormat string

	// DohMethod controls HTTP method used for sending DoH requests. Supported values are "post" and "get". Default is "post".
	DohMethod string
	// DohProtocol controls HTTP protocol version used fo sending DoH requests. Supported values are "1.1", "2" and "3". Default is "1.1".
	DohProtocol string

	// Insecure disables server TLS certificate validation. Applicable for DoT, DoH and DoQ.
	Insecure bool

	// ProgressBar controls whether the progress bar is printed.
	ProgressBar bool

	// Queries list of domains and data sources to be used in Benchmark. It can contain a local file data source referenced using @<file-path>, for example @data/2-domains.
	// It can also be data source file accessible using HTTP, like https://raw.githubusercontent.com/Tantalor93/dnspyre/master/data/1000-domains, in that case the file will be downloaded and saved in-memory.
	// These data sources can be combined, for example "google.com @data/2-domains https://raw.githubusercontent.com/Tantalor93/dnspyre/master/data/2-domains".
	Queries []string

	// RequestLogEnabled controls whether the Benchmark requests will be logged. Requests are logged into the file specified by Benchmark.RequestLogPath field.
	RequestLogEnabled bool

	// RequestLogPath specifies file where the request logs will be logged. If the file does not exist, it is created.
	// If it exists, the request logs are appended to the file.
	RequestLogPath string

	// SeparateWorkerConnections controls whether the concurrent workers will try to share connections to the server or not. When set true,
	// the workers will NOT share connections and each worker will have separate connection.
	SeparateWorkerConnections bool

	// Writer used for writing benchmark execution logs and results. Default is os.Stdout.
	Writer io.Writer

	// RequestDelay configures delay between each DNS request. Either constant delay can be configured (e.g. 2s) or randomized delay can be configured (e.g. 1s-2s).
	RequestDelay string

	// PrometheusMetricsAddr configures address for Prometheus metrics endpoint.
	PrometheusMetricsAddr string

	// internal variable so we do not have to parse the address with each request.
	useDoH            bool
	useQuic           bool
	requestDelayStart time.Duration
	requestDelayEnd   time.Duration
}

type queryFunc func(context.Context, *dns.Msg) (*dns.Msg, error)

// init validates and normalizes Benchmark settings.
func (b *Benchmark) init() error {
	if b.Writer == nil {
		b.Writer = os.Stdout
	}

	if len(b.Server) == 0 {
		return errors.New("server for benchmarking must not be empty")
	}

	b.useDoH, _ = isHTTPUrl(b.Server)
	b.useQuic = strings.HasPrefix(b.Server, "quic://")
	if b.useQuic {
		b.Server = strings.TrimPrefix(b.Server, "quic://")
	}

	if b.useDoH {
		parsedURL, err := url.Parse(b.Server)
		if err != nil {
			return err
		}
		if len(parsedURL.Path) == 0 {
			b.Server += "/dns-query"
		}
	}

	b.addPortIfMissing()

	if b.Count == 0 && b.Duration == 0 {
		b.Count = 1
	}

	if b.Duration > 0 && b.Count > 0 {
		return errors.New("--number and --duration is specified at once, only one can be used")
	}

	if b.HistMax == 0 {
		b.HistMax = b.RequestTimeout
	}

	if b.Edns0 != 0 && (b.Edns0 < 512 || b.Edns0 > 4096) {
		return errors.New("--edns0 must have value between 512 and 4096")
	}

	if len(b.EdnsOpt) != 0 {
		split := strings.Split(b.EdnsOpt, ":")
		if len(split) != 2 {
			return errors.New("--ednsopt is not in correct format")
		}
		_, err := hex.DecodeString(split[1])
		if err != nil {
			return errors.New("--ednsopt is not in correct format, data is not hexadecimal string")
		}
		_, err = strconv.ParseUint(split[0], 10, 16)
		if err != nil {
			return errors.New("--ednsopt is not in correct format, code is not a decimal number")
		}
	}

	if b.RequestLogEnabled && len(b.RequestLogPath) == 0 {
		b.RequestLogPath = DefaultRequestLogPath
	}

	if err := b.parseRequestDelay(); err != nil {
		return err
	}

	return nil
}

// Run executes benchmark, if benchmark is unable to start the error is returned, otherwise array of results from parallel benchmark goroutines is returned.
func (b *Benchmark) Run(ctx context.Context) ([]*ResultStats, error) {
	color.NoColor = !b.Color

	if err := b.init(); err != nil {
		return nil, err
	}

	if b.RequestLogEnabled {
		file, err := os.OpenFile(b.RequestLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		log.SetOutput(file)
	}

	if len(b.PrometheusMetricsAddr) != 0 {
		// nolint:gosec
		server := http.Server{
			Addr:    b.PrometheusMetricsAddr,
			Handler: promhttp.Handler(),
		}
		defer server.Shutdown(ctx)
		go func() {
			if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				printutils.ErrFprintf(b.Writer, "Failed to start Prometheus metrics server at %s: %v\n", b.PrometheusMetricsAddr, err)
			}
		}()
	}

	questions, err := b.prepareQuestions()
	if err != nil {
		return nil, err
	}

	if b.Duration != 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, b.Duration)
		ctx = timeoutCtx
		defer cancel()
	}

	if !b.Silent && !b.JSON {
		printutils.NeutralFprintf(b.Writer, "Using %s hostnames\n", printutils.HighlightSprint(len(questions)))
	}

	var qTypes []uint16
	for _, v := range b.Types {
		qTypes = append(qTypes, dns.StringToType[v])
	}

	queryFactory := workerQueryFactory(b)

	limits := ""
	var limit ratelimit.Limiter
	if b.Rate > 0 {
		limit = ratelimit.New(b.Rate)
		if b.RateLimitWorker == 0 {
			limits = fmt.Sprintf("(limited to %s QPS overall)", printutils.HighlightSprint(b.Rate))
		} else {
			limits = fmt.Sprintf("(limited to %s QPS overall and %s QPS per concurrent worker)",
				printutils.HighlightSprint(b.Rate), printutils.HighlightSprint(b.RateLimitWorker))
		}
	}
	if b.Rate == 0 && b.RateLimitWorker > 0 {
		limits = fmt.Sprintf("(limited to %s QPS per concurrent worker)", printutils.HighlightSprint(b.RateLimitWorker))
	}

	if !b.Silent && !b.JSON {
		network := b.network()
		printutils.NeutralFprintf(b.Writer, "Benchmarking %s via %s with %s concurrent requests %s\n",
			printutils.HighlightSprint(b.Server), printutils.HighlightSprint(network), printutils.HighlightSprint(b.Concurrency), limits)
	}

	var bar *progressbar.ProgressBar
	var incrementBar bool
	if repetitions := b.Count * int64(b.Concurrency) * int64(len(b.Types)) * int64(len(questions)); !b.Silent && b.ProgressBar && repetitions >= 100 {
		fmt.Fprintln(os.Stderr)
		if b.Probability < 1.0 {
			// show spinner when Benchmark.Probability is less than 1.0, because the actual number of repetitions is not known
			repetitions = -1
		}
		bar = progressbar.Default(repetitions, "Progress:")
		incrementBar = true
	}
	if !b.Silent && b.ProgressBar && b.Duration >= 10*time.Second {
		fmt.Fprintln(os.Stderr)
		bar = progressbar.Default(int64(b.Duration.Seconds()), "Progress:")
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-ticker.C:
					bar.Add(1)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	stats := make([]*ResultStats, b.Concurrency)

	var wg sync.WaitGroup
	var w uint32
	for w = 0; w < b.Concurrency; w++ {
		st := newResultStats(b)
		stats[w] = st

		wg.Add(1)
		go func(workerID uint32, st *ResultStats) {
			defer func() {
				wg.Done()
			}()

			// create a new lock free rand source for this goroutine
			// nolint:gosec
			rando := rand.New(rand.NewSource(time.Now().UnixNano()))

			var workerLimit ratelimit.Limiter
			if b.RateLimitWorker > 0 {
				workerLimit = ratelimit.New(b.RateLimitWorker)
			}

			query := queryFactory()

			for i := int64(0); i < b.Count || b.Duration != 0; i++ {
				for _, q := range questions {
					for _, qt := range qTypes {
						if ctx.Err() != nil {
							return
						}
						if rando.Float64() > b.Probability {
							continue
						}
						if limit != nil {
							if err := checkLimit(ctx, limit); err != nil {
								return
							}
						}
						if workerLimit != nil {
							if err := checkLimit(ctx, workerLimit); err != nil {
								return
							}
						}

						req := dns.Msg{}
						req.RecursionDesired = b.Recurse

						req.Question = make([]dns.Question, 1)
						question := dns.Question{Name: q, Qtype: qt, Qclass: dns.ClassINET}
						req.Question[0] = question

						if b.useQuic {
							req.Id = 0
						} else {
							// nolint:gosec
							req.Id = uint16(rand.Intn(1 << 16))
						}

						if b.Edns0 > 0 {
							req.SetEdns0(b.Edns0, false)
						}
						if ednsOpt := b.EdnsOpt; len(ednsOpt) > 0 {
							addEdnsOpt(&req, ednsOpt)
						}
						if b.DNSSEC {
							edns0 := req.IsEdns0()
							if edns0 == nil {
								req.SetEdns0(DefaultEdns0BufferSize, false)
								edns0 = req.IsEdns0()
							}
							edns0.SetDo(true)
						}

						start := time.Now()

						reqTimeoutCtx, cancel := context.WithTimeout(ctx, b.RequestTimeout)
						resp, err := query(reqTimeoutCtx, &req)
						cancel()
						if deadline, deadlineSet := reqTimeoutCtx.Deadline(); err != nil && deadlineSet && start.After(deadline) {
							// Benchmark was cancelled before sending request, do not count this query results and end the worker
							return
						}
						dur := time.Since(start)
						if b.RequestLogEnabled {
							logRequest(workerID, req, resp, err, dur)
						}
						st.record(&req, resp, err, start, dur)
						b.measureProm(req, resp, dur, err)

						if incrementBar {
							bar.Add(1)
						}

						b.delay(ctx, rando)
					}
				}
			}
		}(w, st)
	}

	wg.Wait()
	if bar != nil {
		_ = bar.Exit()
	}

	return stats, nil
}

func (b *Benchmark) measureProm(req dns.Msg, resp *dns.Msg, time time.Duration, err error) {
	if len(b.PrometheusMetricsAddr) == 0 {
		return
	}
	if resp != nil {
		rcode := dns.RcodeToString[resp.Rcode]
		respType := dns.TypeToString[resp.Question[0].Qtype]
		dnsResponseTotalMetrics.WithLabelValues(respType, rcode).Inc()
	}
	if err != nil {
		errorsTotalMetrics.WithLabelValues().Inc()
	}
	reqType := dns.TypeToString[req.Question[0].Qtype]
	dnsRequestsDurationMetrics.WithLabelValues(reqType).Observe(time.Seconds())
}

func (b *Benchmark) delay(ctx context.Context, rando *rand.Rand) {
	switch {
	case b.requestDelayStart > 0 && b.requestDelayEnd > 0:
		delay := time.Duration(rando.Int63n(int64(b.requestDelayEnd-b.requestDelayStart))) + b.requestDelayStart
		waitFor(ctx, delay)
	case b.requestDelayStart > 0:
		waitFor(ctx, b.requestDelayStart)
	default:
	}
}

func waitFor(ctx context.Context, dur time.Duration) {
	timer := time.NewTimer(dur)
	defer timer.Stop()

	select {
	case <-timer.C:
		// slept for requested duration
	case <-ctx.Done():
		// sleep interrupted
	}
}

func (b *Benchmark) network() string {
	if b.useDoH {
		_, network := isHTTPUrl(b.Server)
		network += "/"
		switch b.DohProtocol {
		case HTTP3Proto:
			network += HTTP3Proto
		case HTTP2Proto:
			network += HTTP2Proto
		case HTTP1Proto:
			fallthrough
		default:
			network += HTTP1Proto
		}

		switch b.DohMethod {
		case PostHTTPMethod:
			network += " (POST)"
			return network
		case GetHTTPMethod:
			network += " (GET)"
			return network
		default:
			network += " (POST)"
			return network
		}
	}

	if b.useQuic {
		return QUICTransport
	}

	network := UDPTransport
	if b.TCP {
		network = TCPTransport
	}
	if b.DOT {
		network = TLSTransport
	}
	return network
}

func addEdnsOpt(m *dns.Msg, ednsOpt string) {
	o := m.IsEdns0()
	if o == nil {
		m.SetEdns0(DefaultEdns0BufferSize, false)
		o = m.IsEdns0()
	}
	s := strings.Split(ednsOpt, ":")
	data, _ := hex.DecodeString(s[1])
	code, _ := strconv.ParseUint(s[0], 10, 16)
	o.Option = append(o.Option, &dns.EDNS0_LOCAL{Code: uint16(code), Data: data})
}

func (b *Benchmark) addPortIfMissing() {
	if b.useDoH {
		// both HTTPS and HTTP are using default ports 443 and 80 if no other port is specified
		return
	}
	if _, _, err := net.SplitHostPort(b.Server); err != nil {
		if b.DOT {
			// https://www.rfc-editor.org/rfc/rfc7858
			b.Server = net.JoinHostPort(b.Server, "853")
			return
		}
		if b.useQuic {
			// https://datatracker.ietf.org/doc/rfc9250
			b.Server = net.JoinHostPort(b.Server, "853")
			return
		}
		b.Server = net.JoinHostPort(b.Server, "53")
		return
	}
}

func isHTTPUrl(s string) (ok bool, network string) {
	if strings.HasPrefix(s, "http://") {
		return true, "http"
	}
	if strings.HasPrefix(s, "https://") {
		return true, "https"
	}
	return false, ""
}

func (b *Benchmark) prepareQuestions() ([]string, error) {
	var questions []string
	for _, q := range b.Queries {
		if ok, _ := isHTTPUrl(q); ok {
			resp, err := client.Get(q)
			if err != nil {
				return nil, fmt.Errorf("failed to download file '%s' with error '%v'", q, err)
			}
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return nil, fmt.Errorf("failed to download file '%s' with status '%s'", q, resp.Status)
			}
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				questions = append(questions, dns.Fqdn(scanner.Text()))
			}
		} else {
			questions = append(questions, dns.Fqdn(q))
		}
	}
	return questions, nil
}

func checkLimit(ctx context.Context, limiter ratelimit.Limiter) error {
	done := make(chan struct{})
	go func() {
		limiter.Take()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Benchmark) parseRequestDelay() error {
	if len(b.RequestDelay) == 0 {
		return nil
	}
	requestDelayRegex := regexp.MustCompile(`^(\d+(?:ms|ns|[smhdw]))(?:-(\d+(?:ms|ns|[smhdw])))?$`)

	durations := requestDelayRegex.FindStringSubmatch(b.RequestDelay)
	if len(durations) != 3 {
		return fmt.Errorf("'%s' has unexpected format, either <GO duration> or <GO duration>-<Go duration> is expected", b.RequestDelay)
	}
	if len(durations[1]) != 0 {
		durationStart, err := time.ParseDuration(durations[1])
		if err != nil {
			return err
		}
		b.requestDelayStart = durationStart
	}
	if len(durations[2]) != 0 {
		durationEnd, err := time.ParseDuration(durations[2])
		if err != nil {
			return err
		}
		b.requestDelayEnd = durationEnd
	}
	if b.requestDelayEnd > 0 && b.requestDelayStart > 0 && b.requestDelayEnd-b.requestDelayStart <= 0 {
		return fmt.Errorf("'%s' is invalid interval, start should be strictly less than end", b.RequestDelay)
	}
	return nil
}
