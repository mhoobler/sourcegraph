// Package search is a service which exposes an API to text search a repo at
// a specific commit.
//
// Architecture Notes:
// * Archive is fetched from gitserver
// * Simple HTTP API exposed
// * Currently no concept of authorization
// * On disk cache of fetched archives to reduce load on gitserver
// * Run search on archive. Rely on OS file buffers
// * Simple to scale up since stateless
// * Use ingress with affinity to increase local cache hit ratio
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/trace"

	"sourcegraph.com/sourcegraph/sourcegraph/pkg/searcher/protocol"

	"github.com/pkg/errors"

	"github.com/gorilla/schema"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/prometheus/client_golang/prometheus"
)

// Service is the search service. It is an http.Handler.
type Service struct {
	Store *Store

	// RequestLog if non-nil will log info per valid search request.
	RequestLog *log.Logger
}

var decoder = schema.NewDecoder()

func init() {
	decoder.IgnoreUnknownKeys(true)
}

// ServeHTTP handles HTTP based search requests
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	running.Inc()
	defer running.Dec()

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	var p protocol.Request
	err = decoder.Decode(&p, r.Form)
	if err != nil {
		http.Error(w, "failed to decode form: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.Deadline != "" {
		var deadline time.Time
		if err := deadline.UnmarshalText([]byte(p.Deadline)); err != nil {
			http.Error(w, "invalid deadline: "+err.Error(), http.StatusBadRequest)
			return
		}
		dctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		ctx = dctx
	}
	if !p.PatternMatchesContent && !p.PatternMatchesPath {
		// BACKCOMPAT: Old frontends send neither of these fields, but we still want to
		// search file content in that case.
		p.PatternMatchesContent = true
	}
	if err = validateParams(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	matches, limitHit, err := s.search(ctx, &p)
	if err != nil {
		code := http.StatusInternalServerError
		if isBadRequest(err) || ctx.Err() == context.Canceled {
			code = http.StatusBadRequest
		} else if isTemporary(err) {
			code = http.StatusServiceUnavailable
		} else {
			log.Printf("internal error serving %#+v: %s", p, err)
		}
		http.Error(w, err.Error(), code)
		return
	}
	if matches == nil {
		// Return an empty list
		matches = make([]protocol.FileMatch, 0)
	}

	w.Header().Set("Content-Type", "application/json")
	resp := protocol.Response{
		Matches:  matches,
		LimitHit: limitHit,
	}
	err = json.NewEncoder(w).Encode(&resp)
	if err != nil {
		// We may have already started writing to w
		http.Error(w, "failed to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *Service) search(ctx context.Context, p *protocol.Request) (matches []protocol.FileMatch, limitHit bool, err error) {
	tr := trace.New("search", fmt.Sprintf("%s@%s", p.Repo, p.Commit))
	tr.LazyPrintf("%s", p.Pattern)

	span, ctx := opentracing.StartSpanFromContext(ctx, "Search")
	ext.Component.Set(span, "service")
	span.SetTag("repo", p.Repo)
	span.SetTag("url", p.URL)
	span.SetTag("commit", p.Commit)
	span.SetTag("pattern", p.Pattern)
	span.SetTag("isRegExp", strconv.FormatBool(p.IsRegExp))
	span.SetTag("isWordMatch", strconv.FormatBool(p.IsWordMatch))
	span.SetTag("isCaseSensitive", strconv.FormatBool(p.IsCaseSensitive))
	span.SetTag("pathPatternsAreRegExps", strconv.FormatBool(p.PathPatternsAreRegExps))
	span.SetTag("pathPatternsAreCaseSensitive", strconv.FormatBool(p.PathPatternsAreCaseSensitive))
	span.SetTag("fileMatchLimit", p.FileMatchLimit)
	span.SetTag("patternMatchesContent", p.PatternMatchesContent)
	span.SetTag("patternMatchesPath", p.PatternMatchesPath)
	span.SetTag("deadline", p.Deadline)
	defer func(start time.Time) {
		code := "200"
		// We often have canceled requests. We do not want to
		// record them as errors to avoid noise
		if ctx.Err() == context.Canceled {
			code = "canceled"
			span.SetTag("err", err)
		} else if err != nil {
			tr.LazyPrintf("error: %v", err)
			tr.SetError()
			ext.Error.Set(span, true)
			span.SetTag("err", err.Error())
			if isBadRequest(err) {
				code = "400"
			} else if isTemporary(err) {
				code = "503"
			} else {
				code = "500"
			}
		}
		tr.LazyPrintf("code=%s matches=%d limitHit=%v", code, len(matches), limitHit)
		tr.Finish()
		requestTotal.WithLabelValues(code).Inc()
		span.LogFields(otlog.Int("matches.len", len(matches)))
		span.SetTag("limitHit", limitHit)
		span.Finish()
		if s.RequestLog != nil {
			errS := ""
			if err != nil {
				errS = " error=" + strconv.Quote(err.Error())
			}
			s.RequestLog.Printf("search request repo=%v commit=%v pattern=%q isRegExp=%v isWordMatch=%v isCaseSensitive=%v patternMatchesContent=%v patternMatchesPath=%v matches=%d code=%s duration=%v%s", p.Repo, p.Commit, p.Pattern, p.IsRegExp, p.IsWordMatch, p.IsCaseSensitive, p.PatternMatchesContent, p.PatternMatchesPath, len(matches), code, time.Since(start), errS)
		}
	}(time.Now())

	rg, err := compile(&p.PatternInfo)
	if err != nil {
		return nil, false, badRequestError{err.Error()}
	}

	if p.FetchTimeout == "" {
		p.FetchTimeout = "500ms"
	}
	fetchTimeout, err := time.ParseDuration(p.FetchTimeout)
	if err != nil {
		return nil, false, err
	}
	prepareCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	path, err := s.Store.prepareZip(prepareCtx, p.GitserverRepo(), p.Commit)
	if err != nil {
		return nil, false, err
	}
	zf, err := s.Store.zipCache.get(path)
	if err != nil {
		return nil, false, err
	}
	defer zf.Close()

	nFiles := uint64(len(zf.Files))
	bytes := int64(len(zf.Data))
	tr.LazyPrintf("files=%d bytes=%d", nFiles, bytes)
	span.LogFields(
		otlog.Uint64("archive.files", nFiles),
		otlog.Int64("archive.size", bytes))
	archiveFiles.Observe(float64(nFiles))
	archiveSize.Observe(float64(bytes))

	return concurrentFind(ctx, rg, zf, p.FileMatchLimit, p.PatternMatchesContent, p.PatternMatchesPath)
}

func validateParams(p *protocol.Request) error {
	if p.Repo == "" {
		return errors.New("Repo must be non-empty")
	}
	// Surprisingly this is the same sanity check used in the git source.
	if len(p.Commit) != 40 {
		return errors.Errorf("Commit must be resolved (Commit=%q)", p.Commit)
	}
	if p.Pattern == "" && p.ExcludePattern == "" && len(p.IncludePatterns) == 0 && p.IncludePattern == "" {
		return errors.New("At least one of pattern and include/exclude pattners must be non-empty")
	}
	return nil
}

const megabyte = float64(1000 * 1000)

var (
	running = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "searcher",
		Subsystem: "service",
		Name:      "running",
		Help:      "Number of running search requests.",
	})
	archiveSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "searcher",
		Subsystem: "service",
		Name:      "archive_size_bytes",
		Help:      "Observes the size when an archive is searched.",
		Buckets:   []float64{1 * megabyte, 10 * megabyte, 100 * megabyte, 500 * megabyte, 1000 * megabyte, 5000 * megabyte},
	})
	archiveFiles = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "searcher",
		Subsystem: "service",
		Name:      "archive_files",
		Help:      "Observes the number of files when an archive is searched.",
		Buckets:   []float64{100, 1000, 10000, 50000, 100000},
	})
	requestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "searcher",
		Subsystem: "service",
		Name:      "request_total",
		Help:      "Number of returned search requests.",
	}, []string{"code"})
)

func init() {
	prometheus.MustRegister(running)
	prometheus.MustRegister(requestTotal)
}

type badRequestError struct{ msg string }

func (e badRequestError) Error() string    { return e.msg }
func (e badRequestError) BadRequest() bool { return true }

func isBadRequest(err error) bool {
	e, ok := errors.Cause(err).(interface {
		BadRequest() bool
	})
	return ok && e.BadRequest()
}

func isTemporary(err error) bool {
	e, ok := errors.Cause(err).(interface {
		Temporary() bool
	})
	return ok && e.Temporary()
}
