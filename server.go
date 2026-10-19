package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Huyvux12/RobinCap/capcut"
)

type server struct {
	cfg       Config
	cc        *capcut.Client
	audioHTTP *http.Client
	devSrc    string
	cache     *lruCache[*capcut.TTSResult]
	limiter   *ipLimiter
	started   time.Time
	reqs      atomic.Int64
}

func newServer(cfg Config, cc *capcut.Client, devSrc string) *server {
	s := &server{cfg: cfg, cc: cc, audioHTTP: capcut.NewHTTPClient(), devSrc: devSrc,
		cache: newLRU[*capcut.TTSResult](cfg.CacheSize, cfg.CacheTTL), started: time.Now()}
	if cfg.RateRPM > 0 {
		s.limiter = newIPLimiter(cfg.RateRPM, cfg.RateBurst)
	}
	return s
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	page := func(name, ctype string) http.HandlerFunc {
		b, err := fs.ReadFile(assets, name)
		if err != nil {
			panic(err)
		}
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", page("web/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /docs", page("web/docs.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /openapi.json", page("openapi.json", "application/json"))
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /voices", s.voices)
	mux.HandleFunc("GET /voices/resolve", s.resolveVoice)

	api := func(cost float64, h http.HandlerFunc) http.Handler { return s.protect(cost, h) }
	mux.Handle("GET /device", api(0, s.device))
	mux.Handle("POST /tts", api(1, s.ttsPost))
	mux.Handle("GET /tts", api(1, s.ttsGet))
	mux.Handle("POST /tts/batch", api(1, s.ttsBatch))
	mux.Handle("POST /tts/dry-run", api(1, s.ttsDryRun))
	mux.Handle("GET /tts/task/{id}", api(1, s.ttsTask))
	mux.Handle("POST /upload", api(2, s.upload))
	mux.Handle("POST /stt", api(3, s.sttPost))
	mux.Handle("POST /stt/task", api(1, s.sttCreate))
	mux.Handle("GET /stt/task/{id}", api(1, s.sttTask))
	return s.recoverer(s.cors(s.logger(mux)))
}

// ---------------------------------------------------------------- middleware

type statusWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *server) logger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		s.reqs.Add(1)
		if r.URL.Path != "/health" {
			log.Printf("%s %s %d %dB %s ip=%s", r.Method, r.URL.Path, sw.code, sw.n, time.Since(t).Round(time.Millisecond), clientIP(r))
		}
	})
}

func (s *server) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic: %v\n%s", v, debug.Stack())
				writeErr(w, 500, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func (s *server) cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		origin := s.cfg.CORSOrigin
		if origin != "*" {
			o := r.Header.Get("Origin")
			origin = ""
			for _, a := range strings.Split(s.cfg.CORSOrigin, ",") {
				if strings.TrimSpace(a) == o {
					origin = o
				}
			}
			hd.Add("Vary", "Origin")
		}
		if origin != "" {
			hd.Set("Access-Control-Allow-Origin", origin)
			hd.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			hd.Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
			hd.Set("Access-Control-Expose-Headers", "X-Cache, X-Elapsed-Ms, X-Duration-Ms, Retry-After")
			hd.Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// protect enforces the optional API key and per-IP rate limit.
func (s *server) protect(cost float64, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey != "" {
			k := r.Header.Get("X-API-Key")
			if k == "" {
				k = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if k == "" {
				k = r.URL.Query().Get("api_key")
			}
			if subtle.ConstantTimeCompare([]byte(k), []byte(s.cfg.APIKey)) != 1 {
				writeErr(w, 401, "missing or invalid API key (X-API-Key header)")
				return
			}
		}
		if cost > 0 {
			if ok, retry := s.limiter.allow(clientIP(r), cost); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
				writeErr(w, 429, "rate limit exceeded")
				return
			}
		}
		h(w, r)
	})
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg, "status": code})
}

// upstreamErr maps SDK errors to HTTP codes.
func upstreamErr(w http.ResponseWriter, err error) {
	var te *capcut.TaskError
	var ae *capcut.APIError
	switch {
	case errors.As(err, &te) && te.Timeout:
		writeErr(w, 504, err.Error())
	case errors.As(err, &te):
		writeErr(w, 422, err.Error())
	case errors.As(err, &ae):
		writeErr(w, 502, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, 504, err.Error())
	default:
		writeErr(w, 502, err.Error())
	}
}

// flexStrings accepts "a" or ["a","b"].
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*f = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("must be a string or an array of strings")
	}
	*f = many
	return nil
}

// flexRate accepts 1.2 or "1.2".
type flexRate string

func (f *flexRate) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexRate(s)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("rate must be a number or string")
	}
	*f = flexRate(strconv.FormatFloat(n, 'f', -1, 64))
	return nil
}

func normRate(r string) (string, error) {
	r = strings.TrimSpace(r)
	if r == "" {
		return "1.0", nil
	}
	f, err := strconv.ParseFloat(r, 64)
	if err != nil || f < 0.2 || f > 4 {
		return "", fmt.Errorf("rate must be between 0.2 and 4.0, got %q", r)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s, nil
}

func boolParam(v string, def bool) bool {
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- basic endpoints

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "version": version, "uptime_sec": int(time.Since(s.started).Seconds()),
		"requests": s.reqs.Load(), "cache": s.cache.Stats(), "tts_hedge_wins": s.cc.HedgeWins(), "auth": s.cfg.APIKey != ""})
}

func (s *server) voices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := capcut.Voices(q.Get("lang"), q.Get("q"))
	writeJSON(w, 200, map[string]any{"count": len(v), "langs": capcut.Langs(), "voices": v})
}

func (s *server) resolveVoice(w http.ResponseWriter, r *http.Request) {
	vt, rid := capcut.ResolveVoice(r.URL.Query().Get("voice"), r.URL.Query().Get("resource_id"))
	var info *capcut.VoiceInfo
	for _, v := range capcut.Voices("", "") {
		if v.VoiceType == vt {
			v := v
			info = &v
			break
		}
	}
	writeJSON(w, 200, map[string]any{"voice_type": vt, "resource_id": rid, "in_catalog": info != nil, "voice": info})
}

func (s *server) device(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"source": s.devSrc, "device": s.cc.Device})
}

// ---------------------------------------------------------------- TTS

type ttsReq struct {
	Text       flexStrings `json:"text"`
	Voice      string      `json:"voice"`
	ResourceID string      `json:"resource_id"`
	Rate       flexRate    `json:"rate"`
	Wait       *bool       `json:"wait"`
	Format     string      `json:"format"`
	NoCache    bool        `json:"no_cache"`
	TimeoutSec float64     `json:"timeout_sec"`
}

type ttsOut struct {
	*capcut.TTSResult
	Cached    bool  `json:"cached"`
	ElapsedMs int64 `json:"elapsed_ms"`
}

func (s *server) validateTexts(texts []string) ([]string, error) {
	out := make([]string, 0, len(texts))
	total := 0
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		total += len([]rune(t))
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("text is required")
	}
	if s.cfg.MaxTextChars > 0 && total > s.cfg.MaxTextChars {
		return nil, fmt.Errorf("text too long: %d chars (max %d); split it or use /tts/batch", total, s.cfg.MaxTextChars)
	}
	return out, nil
}

func cacheKey(texts []string, voice, rid, rate string) string {
	vt, r := capcut.ResolveVoice(voice, rid)
	return vt + "\x1f" + r + "\x1f" + rate + "\x1f" + strings.Join(texts, "\x1e")
}

func (s *server) timeout(sec float64) time.Duration {
	if sec > 0 && sec <= 300 {
		return time.Duration(sec * float64(time.Second))
	}
	return s.cfg.TTSTimeout
}

// synth runs generate_speech through the LRU/singleflight cache.
func (s *server) synth(ctx context.Context, texts []string, voice, rid, rate string, noCache bool, to time.Duration) (*capcut.TTSResult, bool, error) {
	// Detach from the caller so a shared (singleflight) call survives one client disconnecting.
	ctx = context.WithoutCancel(ctx)
	run := func() (*capcut.TTSResult, error) {
		return s.cc.GenerateSpeech(ctx, texts, voice, rid, rate, 400*time.Millisecond, to)
	}
	if noCache {
		res, err := run()
		if err == nil {
			s.cache.Put(cacheKey(texts, voice, rid, rate), res)
		}
		return res, false, err
	}
	return s.cache.Do(cacheKey(texts, voice, rid, rate), run)
}

func (s *server) handleTTS(w http.ResponseWriter, r *http.Request, req ttsReq) {
	t0 := time.Now()
	texts, err := s.validateTexts(req.Text)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rate, err := normRate(string(req.Rate))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	format := strings.ToLower(req.Format)
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "mp3" {
		writeErr(w, 400, "format must be json or mp3")
		return
	}
	if req.Wait != nil && !*req.Wait {
		if format == "mp3" {
			writeErr(w, 400, "format=mp3 requires wait=true")
			return
		}
		cr, err := s.cc.CreateTTSTask(r.Context(), texts, req.Voice, req.ResourceID, rate)
		if err != nil {
			upstreamErr(w, err)
			return
		}
		if len(cr.Data.Tasks) == 0 {
			writeErr(w, 502, "no task returned")
			return
		}
		t := cr.Data.Tasks[0]
		vt, rid := capcut.ResolveVoice(req.Voice, req.ResourceID)
		writeJSON(w, 202, map[string]any{"task_id": t.ID, "token": t.Token, "bind_id": t.BindID, "status": orStr(t.Status, "queued"),
			"voice": vt, "resource_id": rid, "rate": rate, "query_url": "/tts/task/" + t.ID + "?token=" + t.Token,
			"elapsed_ms": time.Since(t0).Milliseconds()})
		return
	}
	res, cached, err := s.synth(r.Context(), texts, req.Voice, req.ResourceID, rate, req.NoCache, s.timeout(req.TimeoutSec))
	if err != nil {
		upstreamErr(w, err)
		return
	}
	w.Header().Set("X-Cache", map[bool]string{true: "HIT", false: "MISS"}[cached])
	if format == "mp3" {
		if err := s.streamMP3(w, r, res, r.URL.Query().Get("download") != ""); err != nil {
			// CDN link may have expired: regenerate once without cache.
			res, _, err2 := s.synth(r.Context(), texts, req.Voice, req.ResourceID, rate, true, s.timeout(req.TimeoutSec))
			if err2 == nil {
				err = s.streamMP3(w, r, res, r.URL.Query().Get("download") != "")
			}
			if err != nil {
				writeErr(w, 502, "fetch audio: "+err.Error())
			}
		}
		return
	}
	w.Header().Set("X-Elapsed-Ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
	writeJSON(w, 200, ttsOut{res, cached, time.Since(t0).Milliseconds()})
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// streamMP3 proxies (and concatenates) the segment MP3s. Returns an error only if
// nothing has been written yet, so the caller may retry.
func (s *server) streamMP3(w http.ResponseWriter, r *http.Request, res *capcut.TTSResult, download bool) error {
	bodies := make([]io.ReadCloser, 0, len(res.Segments))
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()
	var total int64
	for _, seg := range res.Segments {
		req, err := http.NewRequestWithContext(r.Context(), "GET", seg.SpeechURL, nil)
		if err != nil {
			return err
		}
		resp, err := s.audioHTTP.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("CDN HTTP %d", resp.StatusCode)
		}
		if total >= 0 && resp.ContentLength >= 0 {
			total += resp.ContentLength
		} else {
			total = -1
		}
		bodies = append(bodies, resp.Body)
	}
	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("X-Duration-Ms", strconv.Itoa(res.DurationMs))
	h.Set("Cache-Control", "private, max-age=3600")
	disp := "inline"
	if download {
		disp = "attachment"
	}
	h.Set("Content-Disposition", disp+`; filename="tts.mp3"`)
	if total > 0 {
		h.Set("Content-Length", strconv.FormatInt(total, 10))
	}
	w.WriteHeader(200)
	for _, b := range bodies {
		if _, err := io.Copy(w, b); err != nil {
			log.Printf("mp3 stream: %v", err)
			return nil
		}
	}
	return nil
}

func (s *server) ttsPost(w http.ResponseWriter, r *http.Request) {
	var req ttsReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if f := r.URL.Query().Get("format"); f != "" && req.Format == "" {
		req.Format = f
	}
	s.handleTTS(w, r, req)
}

func (s *server) ttsGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := ttsReq{Text: q["text"], Voice: q.Get("voice"), ResourceID: q.Get("resource_id"), Rate: flexRate(q.Get("rate")),
		Format: q.Get("format"), NoCache: boolParam(q.Get("no_cache"), false)}
	if v := q.Get("wait"); v != "" {
		b := boolParam(v, true)
		req.Wait = &b
	}
	req.TimeoutSec, _ = strconv.ParseFloat(q.Get("timeout_sec"), 64)
	s.handleTTS(w, r, req)
}

func (s *server) ttsDryRun(w http.ResponseWriter, r *http.Request) {
	var req ttsReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	texts, err := s.validateTexts(req.Text)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rate, err := normRate(string(req.Rate))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rq, err := s.cc.BuildTTSNewRequest(texts, req.Voice, req.ResourceID, rate)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"method": rq.Method, "url": rq.URL, "headers": rq.Headers.Map(), "body": json.RawMessage(rq.Body)})
}

type batchReq struct {
	Texts       []string `json:"texts"`
	Voice       string   `json:"voice"`
	ResourceID  string   `json:"resource_id"`
	Rate        flexRate `json:"rate"`
	Concurrency int      `json:"concurrency"`
	NoCache     bool     `json:"no_cache"`
	TimeoutSec  float64  `json:"timeout_sec"`
}

type batchItem struct {
	Index      int    `json:"index"`
	Text       string `json:"text"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	SpeechURL  string `json:"speech_url,omitempty"`
	DurationMs int    `json:"duration_ms"`
	TaskID     string `json:"task_id,omitempty"`
	Cached     bool   `json:"cached"`
	ElapsedMs  int64  `json:"elapsed_ms"`
}

func (s *server) ttsBatch(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req batchReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(req.Texts) == 0 {
		writeErr(w, 400, "texts is required")
		return
	}
	if len(req.Texts) > s.cfg.BatchMax {
		writeErr(w, 400, fmt.Sprintf("too many texts: %d (max %d)", len(req.Texts), s.cfg.BatchMax))
		return
	}
	// extra rate-limit cost: 1 token per 4 texts beyond the first 4
	if extra := float64(len(req.Texts)-4) / 4; extra > 0 {
		if ok, retry := s.limiter.allow(clientIP(r), extra); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			writeErr(w, 429, "rate limit exceeded for batch size")
			return
		}
	}
	rate, err := normRate(string(req.Rate))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	conc := req.Concurrency
	if conc <= 0 || conc > s.cfg.BatchConcurrency {
		conc = s.cfg.BatchConcurrency
	}
	to := s.timeout(req.TimeoutSec)
	items := make([]batchItem, len(req.Texts))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				st := time.Now()
				it := batchItem{Index: idx, Text: req.Texts[idx]}
				texts, err := s.validateTexts([]string{req.Texts[idx]})
				if err == nil {
					var res *capcut.TTSResult
					res, it.Cached, err = s.synth(r.Context(), texts, req.Voice, req.ResourceID, rate, req.NoCache, to)
					if err == nil {
						it.OK, it.SpeechURL, it.DurationMs, it.TaskID = true, res.SpeechURL, res.DurationMs, res.TaskID
					}
				}
				if err != nil {
					it.Error = err.Error()
				}
				it.ElapsedMs = time.Since(st).Milliseconds()
				items[idx] = it
			}
		}()
	}
	for i := range req.Texts {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	ok, total := 0, 0
	for _, it := range items {
		if it.OK {
			ok++
			total += it.DurationMs
		}
	}
	vt, rid := capcut.ResolveVoice(req.Voice, req.ResourceID)
	writeJSON(w, 200, map[string]any{"count": len(items), "ok": ok, "failed": len(items) - ok, "concurrency": conc,
		"voice": vt, "resource_id": rid, "rate": rate, "total_duration_ms": total,
		"elapsed_ms": time.Since(t0).Milliseconds(), "results": items})
}

func (s *server) ttsTask(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query()
	if q.Get("token") == "" {
		writeErr(w, 400, "token query parameter is required")
		return
	}
	qr, err := s.cc.QueryTTSTask(r.Context(), id, q.Get("token"), q.Get("bind_id"))
	if err != nil {
		upstreamErr(w, err)
		return
	}
	res, perr := capcut.ParseTTSResult(qr)
	out := map[string]any{"task_id": id, "result": res}
	if res != nil {
		out["status"] = res.Status
	}
	if perr != nil {
		out["error"] = perr.Error()
	}
	if boolParam(q.Get("raw"), false) {
		out["raw"] = json.RawMessage(qr.Raw)
	}
	writeJSON(w, 200, out)
}

// ---------------------------------------------------------------- upload / STT

func (s *server) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	max := int64(s.cfg.MaxUploadMB) << 20
	r.Body = http.MaxBytesReader(w, r.Body, max+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, 400, "expected multipart/form-data with a 'file' field (max "+strconv.Itoa(s.cfg.MaxUploadMB)+" MB): "+err.Error())
		return nil, "", false
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "missing 'file' field")
		return nil, "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		writeErr(w, 400, err.Error())
		return nil, "", false
	}
	if int64(len(data)) > max {
		writeErr(w, 413, fmt.Sprintf("file too large (max %d MB)", s.cfg.MaxUploadMB))
		return nil, "", false
	}
	return data, hdr.Filename, true
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	data, name, ok := s.readUpload(w, r)
	if !ok {
		return
	}
	up, err := s.cc.UploadBytes(r.Context(), data)
	if err != nil {
		upstreamErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"filename": name, "upload": up, "elapsed_ms": time.Since(t0).Milliseconds()})
}

func (s *server) sttOpts(get func(string) string) capcut.STTOptions {
	return capcut.STTOptions{Language: orStr(get("language"), s.cfg.DefaultSTTLang),
		TranslationLanguage: orStr(get("translation_language"), "vi-VN"),
		UseTranslation:      boolParam(get("use_translation"), false)}
}

func writeSubs(w http.ResponseWriter, format string, out map[string]any, subs capcut.SubtitleResult) {
	if format == "srt" {
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
		w.Header().Set("Content-Disposition", `inline; filename="transcript.srt"`)
		_, _ = io.WriteString(w, subs.SRT())
		return
	}
	out["subtitles"] = subs
	out["srt"] = subs.SRT()
	writeJSON(w, 200, out)
}

func (s *server) sttPost(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	data, name, ok := s.readUpload(w, r)
	if !ok {
		return
	}
	get := func(k string) string { return orStr(r.FormValue(k), r.URL.Query().Get(k)) }
	opts := s.sttOpts(get)
	format := strings.ToLower(get("format"))
	to := s.cfg.STTTimeout
	if !boolParam(get("wait"), true) {
		up, err := s.cc.UploadBytes(r.Context(), data)
		if err != nil {
			upstreamErr(w, err)
			return
		}
		cr, err := s.cc.CreateSTTTask(r.Context(), up.Vid, up.MD5, up.DurationMs, opts)
		if err != nil {
			upstreamErr(w, err)
			return
		}
		if len(cr.Data.Tasks) == 0 {
			writeErr(w, 502, "no STT task returned")
			return
		}
		t := cr.Data.Tasks[0]
		writeJSON(w, 202, map[string]any{"filename": name, "upload": up, "task_id": t.ID, "token": t.Token, "status": orStr(t.Status, "queued"),
			"query_url": "/stt/task/" + t.ID + "?token=" + t.Token, "elapsed_ms": time.Since(t0).Milliseconds()})
		return
	}
	up, qr, err := s.cc.TranscribeBytes(r.Context(), data, opts, 2*time.Second, to)
	if err != nil {
		upstreamErr(w, err)
		return
	}
	subs, err := capcut.ExtractSubtitles(qr)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	t := qr.Data.Tasks[0]
	writeSubs(w, format, map[string]any{"filename": name, "upload": up, "task_id": t.ID, "status": t.Status, "options": opts,
		"elapsed_ms": time.Since(t0).Milliseconds()}, subs)
}

type sttCreateReq struct {
	Vid                 string `json:"vid"`
	MD5                 string `json:"md5"`
	DurationMs          int    `json:"duration_ms"`
	Language            string `json:"language"`
	TranslationLanguage string `json:"translation_language"`
	UseTranslation      bool   `json:"use_translation"`
	Wait                bool   `json:"wait"`
	Format              string `json:"format"`
}

// sttCreate creates an STT task for an already-uploaded vid (CLI stt-new).
func (s *server) sttCreate(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req sttCreateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Vid == "" || req.MD5 == "" {
		writeErr(w, 400, "vid and md5 are required (from POST /upload)")
		return
	}
	opts := capcut.STTOptions{Language: orStr(req.Language, s.cfg.DefaultSTTLang), TranslationLanguage: orStr(req.TranslationLanguage, "vi-VN"), UseTranslation: req.UseTranslation}
	cr, err := s.cc.CreateSTTTask(r.Context(), req.Vid, req.MD5, req.DurationMs, opts)
	if err != nil {
		upstreamErr(w, err)
		return
	}
	if len(cr.Data.Tasks) == 0 {
		writeErr(w, 502, "no STT task returned")
		return
	}
	t := cr.Data.Tasks[0]
	if !req.Wait {
		writeJSON(w, 202, map[string]any{"task_id": t.ID, "token": t.Token, "status": orStr(t.Status, "queued"),
			"query_url": "/stt/task/" + t.ID + "?token=" + t.Token, "elapsed_ms": time.Since(t0).Milliseconds()})
		return
	}
	qr, err := s.cc.Poll(r.Context(), "stt", t.ID, t.Token, "", 2*time.Second, s.cfg.STTTimeout)
	if err != nil {
		upstreamErr(w, err)
		return
	}
	subs, err := capcut.ExtractSubtitles(qr)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeSubs(w, strings.ToLower(req.Format), map[string]any{"task_id": t.ID, "token": t.Token, "status": qr.Data.Tasks[0].Status,
		"elapsed_ms": time.Since(t0).Milliseconds()}, subs)
}

func (s *server) sttTask(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query()
	if q.Get("token") == "" {
		writeErr(w, 400, "token query parameter is required")
		return
	}
	qr, err := s.cc.QuerySTTTask(r.Context(), id, q.Get("token"), q.Get("bind_id"))
	if err != nil {
		upstreamErr(w, err)
		return
	}
	out := map[string]any{"task_id": id, "status": ""}
	if len(qr.Data.Tasks) > 0 {
		out["status"] = qr.Data.Tasks[0].Status
		out["progress"] = qr.Data.Tasks[0].Progress
	}
	if boolParam(q.Get("raw"), false) {
		out["raw"] = json.RawMessage(qr.Raw)
	}
	st, _ := out["status"].(string)
	if st != "success" && st != "succeed" {
		writeJSON(w, 200, out)
		return
	}
	subs, err := capcut.ExtractSubtitles(qr)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeSubs(w, strings.ToLower(q.Get("format")), out, subs)
}
