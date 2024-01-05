package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/influxdata/tdigest"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/stat"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var conf Config
	conf.Parse()
	if conf.Help {
		fmt.Println(conf.Usage())
		return
	}
	err := conf.Validate()
	if err != nil {
		fmt.Println(err)
		return
	}

	var durInfo string
	if conf.RampUpDuration > 0 {
		durInfo = fmt.Sprintf("(%s ramp up + %s test)", conf.RampUpDuration, conf.Duration)
	}
	fmt.Printf("Running %s%s test with %d concurrency\n  (url: %s)\n",
		(conf.RampUpDuration + conf.Duration).String(), durInfo, conf.Concurrency, conf.URL)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		s, err := Run(ctx, &conf)
		if err != nil {
			fmt.Println(err)
		}
		if s != nil {
			fmt.Println(s.Report())
		}
	}()
	defer func() { <-doneCh }()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	select {
	case <-quit:
		cancel()
	case <-doneCh:
	case <-ctx.Done():
	}
}

func Run(ctx context.Context, conf *Config) (s *Statistic, err error) {
	err = conf.Validate()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, conf.Duration+conf.RampUpDuration)
	defer cancel()

	var tick <-chan time.Time
	if conf.RampUpDuration > 0 {
		step := conf.RampUpDuration / time.Duration(conf.Concurrency)
		ticker := time.NewTicker(step)
		tick = ticker.C
		defer ticker.Stop()
	} else {
		t := make(chan time.Time)
		close(t)
		tick = t
	}
	client := NewClient(conf)
	s = NewStatistic(conf)
	defer s.Done()

	wg := &errgroup.Group{}
	wg.Go(func() error {
		atomic.AddInt32(&s.concurrency, 1)
		wg.Go(func() error {
			return worker(ctx, conf, client, s)
		})
		for atomic.LoadInt32(&s.concurrency) < int32(conf.Concurrency) {
			select {
			case <-tick:
				atomic.AddInt32(&s.concurrency, 1)
				wg.Go(func() error {
					return worker(ctx, conf, client, s)
				})
			case <-ctx.Done():
			}
		}

		select {
		case <-ctx.Done():
		}
		return nil
	})

	err = wg.Wait()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return s, nil
}

type StringsList []string

func (i *StringsList) String() string {
	return strings.Join(*i, ", ")
}

func (i *StringsList) Set(value string) error {
	*i = append(*i, value)
	return nil
}

type Config struct {
	Concurrency    int
	Duration       time.Duration
	RampUpDuration time.Duration
	Method         string
	URL            string
	Headers        StringsList
	Data           string
	MaxRedirects   int
	LogPath        string
	Debug          bool
	Help           bool
}

func (conf *Config) Usage() string {
	return "Usage: demo [options...] <url>\n" +
		"  -c Maximum concurrency (default 1)\n" +
		"  -duration Duration of test (default 30s)\n" +
		"  -ramp_up_duration Time taken to gradually increase to the maximum concurrency\n" +
		"  -X Specify request method to use (default \"GET\")\n" +
		"  -H HTTP headers\n" +
		"  -d HTTP POST data\n" +
		"  -max_redirects Maximum redirect times\n" +
		"  -debug Enable debug log\n" +
		"  -log_path File path for logging (default \"stdout\")"
}

func (conf *Config) Parse() {
	flag.IntVar(&conf.Concurrency, "c", 1, "Maximum concurrency")
	flag.DurationVar(&conf.Duration, "duration", 30*time.Second, "Duration of test")
	flag.DurationVar(&conf.RampUpDuration, "ramp_up_duration", 0*time.Second, "Time taken to gradually increase to the maximum concurrency")
	flag.StringVar(&conf.Method, "X", "GET", "Specify request method to use")
	flag.Var(&conf.Headers, "H", "HTTP headers")
	flag.StringVar(&conf.Data, "d", "", "HTTP POST data")
	flag.IntVar(&conf.MaxRedirects, "max_redirects", 0, "Maximum redirect times")
	flag.StringVar(&conf.LogPath, "log_path", "stdout", "File path for logging")
	flag.BoolVar(&conf.Debug, "debug", false, "Enable debug log")
	flag.BoolVar(&conf.Help, "help", false, "Get help for commands")
	flag.Parse()
	conf.URL = flag.Arg(0)
}

func (conf *Config) Validate() error {
	if conf.Concurrency <= 0 {
		return errors.New("maximum concurrency must be greater then 0")
	}
	if conf.Duration < 0 {
		return errors.New("duration must be greater then or equal to 0s")
	}
	if conf.RampUpDuration < 0 {
		return errors.New("ramp up duration must be greater then or equal to 0s")
	}
	if conf.RampUpDuration+conf.Duration <= 0 {
		return errors.New("test duration must be greater then 0s")
	}
	if conf.MaxRedirects < 0 {
		return errors.New("maximum redirect times must be greater then or equal to 0")
	}
	if conf.URL == "" {
		return errors.New("required url")
	}
	return nil
}

func NewClient(conf *Config) (client *http.Client) {
	client = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   conf.Concurrency,
			MaxConnsPerHost:       conf.Concurrency,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= conf.MaxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return client
}

type Statistic struct {
	// During ramp up latency
	RampRequests int64
	RampErrors   int64

	// After ramp up latency
	Requests     int64
	Errors       int64
	ReadSize     uint64
	TotalLatency time.Duration
	MaxLatency   time.Duration

	Records []Record

	ch             chan *recordingData
	doneCh         chan struct{}
	logger         *zap.Logger
	concurrency    int32
	maxConcurrency int32
	createdAt      time.Time
	duration       time.Duration
	rampUpDuration time.Duration
}

type Record struct {
	Error     bool
	Latency   time.Duration
	CreatedAt time.Time
}

type recordingData struct {
	err         error
	latency     time.Duration
	readSize    int
	concurrency int32
	createdAt   time.Time
}

func NewStatistic(conf *Config) (s *Statistic) {
	s = &Statistic{
		Records:        make([]Record, 0, 1000000),
		ch:             make(chan *recordingData, 1000000),
		doneCh:         make(chan struct{}),
		maxConcurrency: int32(conf.Concurrency),
		createdAt:      time.Now(),
	}
	if conf.LogPath == "" {
		s.logger = zap.NewNop()
	} else {
		lv := zap.NewAtomicLevelAt(zap.InfoLevel)
		if conf.Debug {
			lv = zap.NewAtomicLevelAt(zap.DebugLevel)
		}
		zap.NewProductionConfig()
		s.logger, _ = zap.Config{
			Level: lv,
			Sampling: &zap.SamplingConfig{
				Initial:    100,
				Thereafter: 100,
			},
			Encoding:         "json",
			EncoderConfig:    zap.NewProductionEncoderConfig(),
			OutputPaths:      []string{conf.LogPath},
			ErrorOutputPaths: []string{conf.LogPath},
		}.Build()
	}
	go s.loop()
	return s
}

func (s *Statistic) record(idx int, err error, latency time.Duration, readSize int) {
	s.ch <- &recordingData{
		err:         err,
		latency:     latency,
		readSize:    readSize,
		concurrency: atomic.LoadInt32(&s.concurrency),
		createdAt:   time.Now(),
	}
}

func (s *Statistic) loop() {
	var rampedUpTime, finishedTime time.Time
	defer func() {
		if !rampedUpTime.IsZero() {
			s.rampUpDuration = rampedUpTime.Sub(s.createdAt)
		}
		if !finishedTime.IsZero() {
			s.duration = finishedTime.Sub(s.createdAt) - s.rampUpDuration
		}
		sort.Slice(s.Records, func(i, j int) bool {
			return s.Records[i].CreatedAt.Before(s.Records[j].CreatedAt)
		})
		close(s.doneCh)
	}()

	for data := range s.ch {
		t := data.createdAt
		s.Records = append(s.Records, Record{
			Error:     data.err != nil,
			Latency:   data.latency,
			CreatedAt: data.createdAt,
		})

		if data.concurrency < s.maxConcurrency {
			s.RampRequests++
			if data.err != nil {
				s.RampErrors++
			}
			if t.After(rampedUpTime) {
				rampedUpTime = t
			}
		} else {
			s.Requests++
			if data.err != nil {
				s.Errors++
			}
			s.ReadSize += uint64(data.readSize)
			s.TotalLatency += data.latency
			if data.latency > s.MaxLatency {
				s.MaxLatency = data.latency
			}
			if t.Before(rampedUpTime) {
				rampedUpTime = t
			}
		}
		if t.After(finishedTime) {
			finishedTime = t
		}
	}
}

func (s *Statistic) Logger() *zap.Logger {
	return s.logger
}

func (s *Statistic) Done() {
	close(s.ch)
	<-s.doneCh
}

func (s *Statistic) Report() string {
	select {
	case <-s.doneCh:
	default:
		panic("can not get report before finish collecting data")
	}

	var report string
	rampUpDuration := s.rampUpDuration.Round(10 * time.Microsecond)
	if rampUpDuration > 0 {
		report += fmt.Sprintf("Ramp up in %s, %d requests, %d errors, %s read",
			rampUpDuration, s.RampRequests, s.RampErrors, humanize.Bytes(s.ReadSize),
		)
	}

	duration := s.duration.Round(10 * time.Microsecond)
	if duration > 0 {
		latencyAvg := (s.TotalLatency / time.Duration(s.Requests)).Round(10 * time.Microsecond)

		latencyMax := s.MaxLatency.Round(10 * time.Microsecond)
		td := tdigest.New()
		f := make([]float64, 0, len(s.Records))
		for _, r := range s.Records {
			td.Add(float64(r.Latency), 1)
			f = append(f, float64(r.Latency))
		}
		stdev := stat.StdDev(f, nil)
		latencyStdev := time.Duration(stdev).Round(10 * time.Microsecond)
		mean := float64(latencyAvg)
		latencySigmaPCT := float64(floats.Count(func(x float64) bool {
			return x >= mean-stdev && x <= mean+stdev
		}, f)) / float64(len(f)) * 100
		latencyP50 := time.Duration(td.Quantile(0.5)).Round(10 * time.Microsecond)
		latencyP75 := time.Duration(td.Quantile(0.75)).Round(10 * time.Microsecond)
		latencyP90 := time.Duration(td.Quantile(0.9)).Round(10 * time.Microsecond)
		latencyP99 := time.Duration(td.Quantile(0.99)).Round(10 * time.Microsecond)
		td.Reset()

		qps := float64(s.Requests) / s.TotalLatency.Seconds()
		f = make([]float64, 0, len(s.Records))
		var ts, q, qpsMax int64
		for _, r := range s.Records {
			if t := r.CreatedAt.Unix(); t != ts {
				if ts != 0 {
					f = append(f, float64(q))
					if q > qpsMax {
						qpsMax = q
					}
					q = 0
				}
				ts = t
			}
			q++
		}
		qpsStdev := stat.StdDev(f, nil)
		qpsSigmaPCT := float64(floats.Count(func(x float64) bool {
			return x >= qps-qpsStdev && x <= qps+qpsStdev
		}, f)) / float64(len(f)) * 100
		td.Reset()
		f = nil

		if report != "" {
			report += "\n"
		}
		report += fmt.Sprintf("Test in %s, %d requests, %d errors, %s read\n"+
			"\tStats\t\tAvg\t\tStdev\t\tMax\t\t+/- Stdev\n"+
			"\tLatency\t\t%s\t\t%s\t\t%s\t\t%.2f%%\n"+
			"\tQPS\t\t%.2f\t\t%.2f\t\t%d\t\t%.2f%%\n"+
			"Latency Distribution\n"+
			"\t50%%\t%s\n"+
			"\t75%%\t%s\n"+
			"\t90%%\t%s\n"+
			"\t99%%\t%s",
			duration, s.Requests, s.Errors, humanize.Bytes(s.ReadSize),
			latencyAvg, latencyStdev, latencyMax, latencySigmaPCT,
			qps, qpsStdev, qpsMax, qpsSigmaPCT,
			latencyP50, latencyP75, latencyP90, latencyP99,
		)
	}

	return report
}

func worker(ctx context.Context, conf *Config, client *http.Client, s *Statistic) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		reqBody := []byte(conf.Data)
		var b io.Reader
		if reqBody != nil {
			b = bytes.NewReader(reqBody)
		}
		req, err := http.NewRequestWithContext(ctx, conf.Method, conf.URL, b)
		if err != nil {
			return err
		}
		startedAt := time.Now()
		resp, err := client.Do(req)
		latency := time.Since(startedAt)
		var statusCode int
		var header http.Header
		var body []byte
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return err
			}
		} else {
			statusCode = resp.StatusCode
			header = resp.Header
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err == nil {
				if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
					err = errors.New("failed")
				}

			}
		}

		s.record(0, err, latency, len(body)+getHTTPHeaderSize(header))
		s.Logger().Debug("Request",
			zap.String("request.url", req.URL.String()),
			zap.Any("request.header", req.Header),
			zap.ByteString("request.body", reqBody),
			zap.Int("status_code", statusCode),
			zap.Any("response.header", header),
			zap.ByteString("response.body", body),
		)
	}
}

func getHTTPHeaderSize(h http.Header) int {
	size := 0
	for k, v := range h {
		size += len(k) + len(": \r\n")
		for _, s := range v {
			size += len(s)
		}
	}
	size += len("\r\n")
	return size
}
