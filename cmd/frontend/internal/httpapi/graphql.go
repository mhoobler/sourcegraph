package httpapi

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/graph-gophers/graphql-go"
	gqlerrors "github.com/graph-gophers/graphql-go/errors"
	"github.com/inconshreveable/log15"
	"github.com/throttled/throttled/v2"
	"github.com/throttled/throttled/v2/store/redigostore"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/honey"
	"github.com/sourcegraph/sourcegraph/internal/redispool"
	"github.com/sourcegraph/sourcegraph/internal/trace"
)

type graphQLQueryParams struct {
	Query         string                 `json:"query"`
	OperationName string                 `json:"operationName"`
	Variables     map[string]interface{} `json:"variables"`
}

func serveGraphQL(schema *graphql.Schema, isInternal bool) func(w http.ResponseWriter, r *http.Request) (err error) {
	rlw := newRateLimitWatcher()
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		if r.Method != "POST" {
			// The URL router should not have routed to this handler if method is not POST, but just in
			// case.
			return errors.New("method must be POST")
		}

		// We use the query to denote the name of a GraphQL request, e.g. for /.api/graphql?Repositories
		// the name is "Repositories".
		requestName := "unknown"
		if r.URL.RawQuery != "" {
			requestName = r.URL.RawQuery
		}
		requestSource := guessSource(r)

		// Used by the prometheus tracer
		r = r.WithContext(trace.WithGraphQLRequestName(r.Context(), requestName))
		r = r.WithContext(trace.WithRequestSource(r.Context(), requestSource))

		if r.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(r.Body)
			if err != nil {
				return err
			}

			r.Body = gzipReader

			defer gzipReader.Close()
		}

		var params graphQLQueryParams
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			return err
		}

		traceData := traceData{
			queryParams:   params,
			isInternal:    isInternal,
			requestName:   requestName,
			requestSource: string(requestSource),
		}

		uid, isIP, anonymous := getUID(r)
		traceData.uid = uid
		traceData.anonymous = anonymous

		cost, costErr := graphqlbackend.EstimateQueryCost(params.Query, params.Variables)
		if costErr != nil {
			log15.Warn("estimating GraphQL cost", "error", costErr)
		}
		traceData.costError = costErr
		traceData.cost = cost

		rl := rlw.getLimiter()
		if rl.enabled {
			limited, result, err := rl.rateLimit(uid, isIP, cost.FieldCount)
			if err != nil {
				log15.Error("checking GraphQL rate limit", "error", err)
				traceData.limitError = err
			} else {
				traceData.limited = limited
				traceData.limitResult = result
			}
		}

		traceData.requestStart = time.Now()
		response := schema.Exec(r.Context(), params.Query, params.OperationName, params.Variables)
		traceData.queryErrors = response.Errors
		traceGraphQL(traceData)
		responseJSON, err := json.Marshal(response)
		if err != nil {
			return err
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(responseJSON)

		return nil
	}
}

type rateLimitWatcher struct {
	rl atomic.Value // *rateLimiter
}

func newRateLimitWatcher() *rateLimitWatcher {
	// TODO: Come up with a non random value here
	const maxBurst = 100

	disabledLimiter := &rateLimiter{
		enabled: false,
	}
	w := &rateLimitWatcher{}
	w.rl.Store(disabledLimiter)

	conf.Watch(func() {
		log15.Debug("Rate limit config updated, applying changes")
		rlc := conf.Get().ApiRatelimit
		if rlc == nil || !rlc.Enabled {
			w.rl.Store(disabledLimiter)
			return
		}
		store, err := redigostore.New(redispool.Cache, "gql:rl", 0)
		if err != nil {
			log15.Warn("error creating ratelimit store", "error", err)
			return
		}
		ipQuota := throttled.RateQuota{
			MaxRate:  throttled.PerHour(rlc.PerIP),
			MaxBurst: maxBurst,
		}
		userQuota := throttled.RateQuota{
			MaxRate:  throttled.PerHour(rlc.PerUser),
			MaxBurst: maxBurst,
		}
		ipLimiter, err := throttled.NewGCRARateLimiter(store, ipQuota)
		if err != nil {
			log15.Warn("error creating ip rate limiter", "error", err)
			return
		}
		userLimiter, err := throttled.NewGCRARateLimiter(store, userQuota)
		if err != nil {
			log15.Warn("error creating user rate limiter", "error", err)
			return
		}

		// TODO: Iterate over overrides

		// Store the new limiter
		w.rl.Store(&rateLimiter{
			enabled:     true,
			ipLimiter:   ipLimiter,
			userLimiter: userLimiter,
		})
	})

	return w
}

func (w *rateLimitWatcher) getLimiter() *rateLimiter {
	l, ok := w.rl.Load().(*rateLimiter)
	if ok {
		return l
	}
	return &rateLimiter{
		enabled: false,
	}
}

type rateLimiter struct {
	enabled     bool
	ipLimiter   *throttled.GCRARateLimiter
	userLimiter *throttled.GCRARateLimiter
	overrides   map[string]*throttled.GCRARateLimiter
}

// rateLimit will check the current rate limit. It is assumed that the caller has already confirmed that
// rate limiting is enabled.
func (rl *rateLimiter) rateLimit(uid string, isIP bool, cost int) (bool, throttled.RateLimitResult, error) {
	if r, ok := rl.overrides[uid]; ok {
		return r.RateLimit(uid, cost)
	}
	if isIP {
		return rl.ipLimiter.RateLimit(uid, cost)
	}
	return rl.userLimiter.RateLimit(uid, cost)
}

type traceData struct {
	queryParams   graphQLQueryParams
	requestStart  time.Time
	uid           string
	anonymous     bool
	isInternal    bool
	requestName   string
	requestSource string
	queryErrors   []*gqlerrors.QueryError

	cost      *graphqlbackend.QueryCost
	costError error

	limited     bool
	limitError  error
	limitResult throttled.RateLimitResult
}

func traceGraphQL(data traceData) {
	if !honey.Enabled() || traceGraphQLQueriesSample <= 0 {
		return
	}

	duration := time.Since(data.requestStart)

	ev := honey.Event("graphql-cost")
	ev.SampleRate = uint(traceGraphQLQueriesSample)

	ev.AddField("query", data.queryParams.Query)
	ev.AddField("variables", data.queryParams.Variables)
	ev.AddField("operationName", data.queryParams.OperationName)

	ev.AddField("anonymous", data.anonymous)
	ev.AddField("uid", data.uid)
	ev.AddField("isInternal", data.isInternal)
	// Honeycomb has built in support for latency if you use milliseconds. We
	// multiply seconds by 1000 here instead of using d.Milliseconds() so that we
	// don't truncate durations of less than 1 millisecond.
	ev.AddField("durationMilliseconds", duration.Seconds()*1000)
	ev.AddField("hasQueryErrors", len(data.queryErrors) > 0)
	ev.AddField("requestName", data.requestName)
	ev.AddField("requestSource", data.requestSource)

	if data.costError != nil {
		ev.AddField("hasCostError", true)
		ev.AddField("costError", data.costError.Error())
	} else {
		ev.AddField("hasCostError", false)
		ev.AddField("cost", data.cost.FieldCount)
		ev.AddField("depth", data.cost.MaxDepth)
		ev.AddField("costVersion", data.cost.Version)
	}

	ev.AddField("rateLimited", data.limited)
	if data.limitError != nil {
		ev.AddField("rateLimitError", data.limitError.Error())
	} else {
		ev.AddField("rateLimit", data.limitResult.Limit)
		ev.AddField("rateLimitRemaining", data.limitResult.Remaining)
	}

	_ = ev.Send()
}

var traceGraphQLQueriesSample = func() int {
	rate, _ := strconv.Atoi(os.Getenv("TRACE_GRAPHQL_QUERIES_SAMPLE"))
	return rate
}()

func getUID(r *http.Request) (uid string, ip bool, anonymous bool) {
	a := actor.FromContext(r.Context())
	anonymous = !a.IsAuthenticated()
	if !anonymous {
		return a.UIDString(), false, anonymous
	}
	if cookie, err := r.Cookie("sourcegraphAnonymousUid"); err == nil && cookie.Value != "" {
		return cookie.Value, false, anonymous
	}
	// The user is anonymous with no cookie, use IP
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip, true, anonymous
	}
	return "unknown", false, anonymous
}

// guessSource guesses the source the request came from (browser, other HTTP client, etc.)
func guessSource(r *http.Request) trace.SourceType {
	userAgent := r.UserAgent()
	for _, guess := range []string{
		"Mozilla",
		"WebKit",
		"Gecko",
		"Chrome",
		"Firefox",
		"Safari",
		"Edge",
	} {
		if strings.Contains(userAgent, guess) {
			return trace.SourceBrowser
		}
	}
	return trace.SourceOther
}
