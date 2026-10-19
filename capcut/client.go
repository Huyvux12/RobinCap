package capcut

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Request is a fully built (signed) HTTP request; useful for dry-runs.
type Request struct {
	Method  string `json:"method"`
	URL     string `json:"url"`
	Headers Header `json:"-"`
	Body    string `json:"body"`
}

// APIError is returned for HTTP >= 400, non-JSON bodies, or ret != "0".
type APIError struct {
	Label      string
	StatusCode int
	Ret        string
	Msg        string
	Body       string
}

func (e *APIError) Error() string {
	if e.Ret != "" {
		return fmt.Sprintf("%s: ret=%s %s", e.Label, e.Ret, e.Msg)
	}
	return fmt.Sprintf("%s HTTP %d: %s", e.Label, e.StatusCode, e.Msg)
}

// TaskError is returned when a task fails or times out.
type TaskError struct {
	Kind, TaskID, Status, Msg string
	Timeout                   bool
}

func (e *TaskError) Error() string {
	if e.Timeout {
		return fmt.Sprintf("%s task %s timed out (last status %q)", e.Kind, e.TaskID, e.Status)
	}
	return fmt.Sprintf("%s task %s %s: %s", e.Kind, e.TaskID, e.Status, e.Msg)
}

type Client struct {
	Device     DeviceConfig
	HTTP       *http.Client
	Entropy    Entropy
	MaxRetries int           // extra attempts on network errors / 5xx / 429
	HedgeAfter time.Duration // submit a duplicate TTS task if not done after this (0 = off)
	MaxHedges  int           // max duplicate tasks per request
	hedgeWins  atomic.Int64
}

// NewHTTPClient returns a keep-alive client tuned for many small calls.
func NewHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute}
}

func NewClient(d DeviceConfig) *Client {
	return &Client{Device: d, HTTP: NewHTTPClient(), Entropy: DefaultEntropy(), MaxRetries: 2, HedgeAfter: 4500 * time.Millisecond, MaxHedges: 2}
}

func (c *Client) sign(path string, q Query, body string, appid bool) *Request {
	u := BaseURL + path + "?" + q.Encode()
	h := BaseHeaders(c.Device, body, appid, c.Entropy)
	if h.Get("sign") == "" {
		h = append(h, KV{"sign", MakeSignHeader(u, c.Device.Appvr, h.Get("device-time"), c.Device.Tdid)})
	}
	return &Request{Method: "POST", URL: u, Headers: h, Body: body}
}

var (
	ttsBabi = Obj{{"feature_entrance", "editor"}, {"feature_entrance_detail", "editor-feature-text_to_speech"},
		{"feature_key", "text_to_speech"}, {"scenario", "video_editor"}}
	sttBabi = Obj{{"feature_entrance", "editor"}, {"feature_entrance_detail", "editor-elements-captions-subtitle_recognition"},
		{"feature_key", "subtitle_recognition"}, {"scenario", "video_editor"}}
)

// BuildTTSNewRequest mirrors build_tts_new_request.
func (c *Client) BuildTTSNewRequest(texts []string, voice, resourceID, rate string) (*Request, error) {
	if len(texts) == 0 {
		return nil, errors.New("texts must contain at least one string")
	}
	if rate == "" {
		rate = "1.0"
	}
	voiceType, resID := ResolveVoice(voice, resourceID)
	blocks := make([]string, len(texts))
	for i, t := range texts {
		blocks[i] = `    <voice name="` + voiceType + `" mock_tone_info="" platform="sami" ` +
			`resource_id="` + resID + `" emotion="" emotion_scale="0" style="" role="" ` +
			`moyin_emotion="" is_clone_tone="false" need_subtitle_timestamp="false">` + "\n" +
			`        <prosody rate="` + rate + `">` + EscapeXML(t) + `</prosody>` + "\n" +
			`    </voice>`
	}
	ssml := `<speak version="1.0" xmlns="http://www.w3.org/2001/10/synthesis" xml:lang="en-US">` + "\n" +
		strings.Join(blocks, "\n") + "\n</speak>"
	extra := CompactJSON(Obj{{"benefit_info", Obj{}}})
	sig, err := MakeTTSPayloadSign(ssml, &extra, c.Device.DeviceID, c.Device.Aid, c.Entropy.Rand)
	if err != nil {
		return nil, err
	}
	payload := Obj{{"audio_format", "mp3"}, {"babi_param", CompactJSON(ttsBabi)}, {"credit_disable", false},
		{"extra_info", extra}, {"need_merge_voice", false}, {"need_subtitle_timestamp", false},
		{"scene", "text_to_speech"}, {"ssml", ssml}, {"sign", sig}}
	bindID := c.Entropy.UUID()
	ctxID := c.Entropy.UUID()
	body := CompactJSON(Obj{{"bind_id", bindID}, {"can_queue", true}, {"enter_from", "text_to_speech"},
		{"tasks", []any{Obj{{"context", ctxID}, {"payload", CompactJSON(payload)},
			{"req_key", "sami_text_to_speech"}, {"task_version", "v3"}}}}})
	return c.sign("/lv/v1/common_task/new", CommonQuery(c.Device, ttsBabi, true), body, true), nil
}

// STTOptions are the STT task parameters.
type STTOptions struct {
	Language            string `json:"language"`
	TranslationLanguage string `json:"translation_language"`
	UseTranslation      bool   `json:"use_translation"`
}

func (o *STTOptions) defaults() {
	if o.Language == "" {
		o.Language = "zh-CN"
	}
	if o.TranslationLanguage == "" {
		o.TranslationLanguage = "vi-VN"
	}
}

// BuildSTTNewRequest mirrors build_stt_new_request.
func (c *Client) BuildSTTNewRequest(vid, md5sum string, durationMs int, o STTOptions) *Request {
	o.defaults()
	if durationMs <= 0 {
		durationMs = 10000
	}
	reqID := c.Entropy.UUID()
	cap := Obj{{"adjust_endtime", 200}, {"audio", vid}, {"audio_type", "vid"}, {"caption_type", 0},
		{"client_request_id", reqID}, {"duration", durationMs}, {"enable_cache", true}, {"enter_from", "asr"},
		{"language", o.Language}, {"max_lines", 1}, {"md5", md5sum},
		{"pack_options", Obj{{"need_attribute", true}}},
		{"songs_info", []any{Obj{{"end_time", float64(durationMs) - 10.334}, {"id", ""}, {"start_time", 0}}}},
		{"translation_language", o.TranslationLanguage}, {"use_translation", o.UseTranslation}, {"words_per_line", 15}}
	bindID := strings.ToUpper(c.Entropy.UUID())
	ctxID := c.Entropy.UUID()
	body := CompactJSON(Obj{{"bind_id", bindID}, {"can_queue", true}, {"enter_from", "asr"},
		{"tasks", []any{Obj{{"context", ctxID}, {"payload", CompactJSON(Obj{{"cap_json", cap}})},
			{"req_key", "cc_audio_subtitle_asr"}, {"task_version", "v3"}}}}})
	return c.sign("/lv/v1/common_task/new", CommonQuery(c.Device, sttBabi, true), body, false)
}

// BuildQueryRequest mirrors build_query_request (mode "tts" or "stt").
func (c *Client) BuildQueryRequest(taskID, token, mode, bindID string) *Request {
	tts := mode == "tts" || mode == "tts-query"
	reqKey := "cc_audio_subtitle_asr"
	if tts {
		reqKey = "sami_text_to_speech"
	}
	body := CompactJSON(Obj{{"tasks", []any{Obj{{"bind_id", bindID}, {"id", taskID}, {"req_key", reqKey},
		{"task_version", "v3"}, {"token", token}}}}})
	return c.sign("/lv/v1/common_task/query", CommonQuery(c.Device, nil, false), body, tts)
}

// ---------------------------------------------------------------- transport

func retryable(status int) bool { return status == 429 || status >= 500 }

// do executes req (rebuilt by build on every attempt) with retries + backoff.
func (c *Client) do(ctx context.Context, label string, build func() (*Request, error), extra http.Header) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			d := time.Duration(200*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(d):
			}
		}
		r, err := build()
		if err != nil {
			return nil, 0, err
		}
		hr, err := http.NewRequestWithContext(ctx, r.Method, r.URL, strings.NewReader(r.Body))
		if err != nil {
			return nil, 0, err
		}
		for _, kv := range r.Headers {
			hr.Header[kv.K] = []string{kv.V.(string)}
		}
		for k, v := range extra {
			hr.Header[k] = v
		}
		resp, err := c.HTTP.Do(hr)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", label, err)
			if ctx.Err() != nil {
				return nil, 0, lastErr
			}
			continue
		}
		b, err := readBody(resp)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s read: %w", label, err)
			continue
		}
		if retryable(resp.StatusCode) && attempt < c.MaxRetries {
			lastErr = &APIError{Label: label, StatusCode: resp.StatusCode, Msg: truncate(string(b), 300)}
			continue
		}
		return b, resp.StatusCode, nil
	}
	return nil, 0, lastErr
}

func readBody(resp *http.Response) ([]byte, error) {
	var r io.Reader = resp.Body
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	case "deflate":
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			return io.ReadAll(zr)
		}
		return io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	}
	return io.ReadAll(io.LimitReader(r, 64<<20))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// checkedJSON mirrors _checked_json_response, plus a ret != "0" check for CapCut endpoints.
func checkedJSON(label string, b []byte, status int, v any, checkRet bool) error {
	if err := json.Unmarshal(b, v); err != nil {
		return &APIError{Label: label, StatusCode: status, Msg: "non-JSON: " + truncate(string(b), 300)}
	}
	if status >= 400 {
		return &APIError{Label: label, StatusCode: status, Msg: truncate(string(b), 500)}
	}
	if checkRet {
		var env struct {
			Ret    json.RawMessage `json:"ret"`
			Errmsg string          `json:"errmsg"`
		}
		_ = json.Unmarshal(b, &env)
		ret := strings.Trim(string(env.Ret), `"`)
		if ret != "" && ret != "0" {
			return &APIError{Label: label, StatusCode: status, Ret: ret, Msg: env.Errmsg}
		}
	}
	return nil
}

// ---------------------------------------------------------------- task API

type Task struct {
	ID            string          `json:"id"`
	BindID        string          `json:"bind_id,omitempty"`
	Token         string          `json:"token,omitempty"`
	Status        string          `json:"status,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Progress      int             `json:"progress,omitempty"`
	FailedNodeKey string          `json:"failed_node_key,omitempty"`
}

type TaskResponse struct {
	Ret    json.RawMessage `json:"ret"`
	Errmsg string          `json:"errmsg"`
	LogID  string          `json:"log_id"`
	Data   struct {
		Tasks []Task `json:"tasks"`
	} `json:"data"`
	Raw json.RawMessage `json:"-"`
}

// PayloadBytes returns the task payload decoded from its JSON-string wrapper.
func (t Task) PayloadBytes() []byte {
	p := bytes.TrimSpace(t.Payload)
	if len(p) > 0 && p[0] == '"' {
		var s string
		if json.Unmarshal(p, &s) == nil {
			return []byte(s)
		}
	}
	return p
}

func (c *Client) taskCall(ctx context.Context, label string, build func() (*Request, error)) (*TaskResponse, error) {
	b, st, err := c.do(ctx, label, build, nil)
	if err != nil {
		return nil, err
	}
	var tr TaskResponse
	if err := checkedJSON(label, b, st, &tr, true); err != nil {
		return nil, err
	}
	tr.Raw = b
	return &tr, nil
}

func (c *Client) CreateTTSTask(ctx context.Context, texts []string, voice, resourceID, rate string) (*TaskResponse, error) {
	if _, err := c.BuildTTSNewRequest(texts, voice, resourceID, rate); err != nil {
		return nil, err
	}
	return c.taskCall(ctx, "create_tts_task", func() (*Request, error) {
		return c.BuildTTSNewRequest(texts, voice, resourceID, rate)
	})
}

func (c *Client) QueryTTSTask(ctx context.Context, id, token, bindID string) (*TaskResponse, error) {
	return c.taskCall(ctx, "query_tts_task", func() (*Request, error) { return c.BuildQueryRequest(id, token, "tts", bindID), nil })
}

func (c *Client) CreateSTTTask(ctx context.Context, vid, md5sum string, durationMs int, o STTOptions) (*TaskResponse, error) {
	return c.taskCall(ctx, "create_stt_task", func() (*Request, error) { return c.BuildSTTNewRequest(vid, md5sum, durationMs, o), nil })
}

func (c *Client) QuerySTTTask(ctx context.Context, id, token, bindID string) (*TaskResponse, error) {
	return c.taskCall(ctx, "query_stt_task", func() (*Request, error) { return c.BuildQueryRequest(id, token, "stt", bindID), nil })
}

func isDone(s string) bool { return s == "success" || s == "succeed" }

// Poll queries until success/succeed, failed, or timeout. Backoff starts fast
// because cached TTS results are usually ready immediately.
func (c *Client) Poll(ctx context.Context, kind string, id, token, bindID string, maxInterval, timeout time.Duration) (*TaskResponse, error) {
	if maxInterval <= 0 {
		maxInterval = time.Second
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := c.QueryTTSTask
	if kind == "stt" {
		q = c.QuerySTTTask
	}
	wait := 200 * time.Millisecond
	last := ""
	for {
		r, err := q(ctx, id, token, bindID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &TaskError{Kind: kind, TaskID: id, Status: last, Timeout: true}
			}
			return nil, err
		}
		if len(r.Data.Tasks) > 0 {
			t := r.Data.Tasks[0]
			last = t.Status
			if isDone(t.Status) {
				return r, nil
			}
			if t.Status == "failed" {
				msg := strings.TrimSpace(string(t.PayloadBytes()))
				if msg == "" || msg == "{}" {
					msg = "upstream gave no reason (failed_node_key=" + t.FailedNodeKey + "); the voice may be unavailable for this account/region"
				}
				return r, &TaskError{Kind: kind, TaskID: id, Status: t.Status, Msg: truncate(msg, 500)}
			}
		}
		select {
		case <-ctx.Done():
			return nil, &TaskError{Kind: kind, TaskID: id, Status: last, Timeout: true}
		case <-time.After(wait):
		}
		if wait = wait * 2; wait > maxInterval {
			wait = maxInterval
		}
	}
}

// ---------------------------------------------------------------- TTS result

type TTSSegment struct {
	Text       string          `json:"text"`
	SpeechURL  string          `json:"speech_url"`
	SpeechVid  string          `json:"speech_vid,omitempty"`
	DurationMs int             `json:"duration_ms"`
	Utterances json.RawMessage `json:"utterances"`
	HitCache   bool            `json:"hit_cache"`
	SpeakerID  string          `json:"speaker_id,omitempty"`
	Code       int             `json:"code"`
	Msg        string          `json:"msg,omitempty"`
}

type TTSResult struct {
	TaskID     string       `json:"task_id"`
	Token      string       `json:"token,omitempty"`
	BindID     string       `json:"bind_id,omitempty"`
	Status     string       `json:"status"`
	Voice      string       `json:"voice"`
	ResourceID string       `json:"resource_id"`
	Rate       string       `json:"rate"`
	SpeechURL  string       `json:"speech_url,omitempty"`
	DurationMs int          `json:"duration_ms"`
	Segments   []TTSSegment `json:"segments"`
}

// ParseTTSResult extracts audio_subtitles from a successful query response.
func ParseTTSResult(r *TaskResponse) (*TTSResult, error) {
	if len(r.Data.Tasks) == 0 {
		return nil, errors.New("no task in response")
	}
	t := r.Data.Tasks[0]
	res := &TTSResult{TaskID: t.ID, BindID: t.BindID, Token: t.Token, Status: t.Status, Segments: []TTSSegment{}}
	if !isDone(t.Status) {
		return res, nil
	}
	var p struct {
		AudioSubtitles []struct {
			TTSSegment
			Duration int `json:"duration"`
		} `json:"audio_subtitles"`
	}
	if err := json.Unmarshal(t.PayloadBytes(), &p); err != nil {
		return nil, fmt.Errorf("parse tts payload: %w", err)
	}
	for _, a := range p.AudioSubtitles {
		s := a.TTSSegment
		s.DurationMs = a.Duration
		if len(s.Utterances) == 0 {
			s.Utterances = json.RawMessage("null")
		}
		res.Segments = append(res.Segments, s)
		res.DurationMs += s.DurationMs
	}
	if len(res.Segments) > 0 {
		res.SpeechURL = res.Segments[0].SpeechURL
	}
	for _, s := range res.Segments {
		if s.SpeechURL == "" {
			msg := s.Msg
			if msg == "" {
				msg = fmt.Sprintf("segment code %d without speech_url", s.Code)
			}
			return res, &TaskError{Kind: "tts", TaskID: t.ID, Status: "invalid", Msg: msg}
		}
	}
	return res, nil
}

// GenerateSpeech submits a TTS task and polls until done (generate_speech).
// CapCut occasionally leaves a task stuck in "processing"; when HedgeAfter > 0
// a duplicate task is submitted after that delay (up to MaxHedges extra) and the
// first one to succeed wins.
func (c *Client) GenerateSpeech(ctx context.Context, texts []string, voice, resourceID, rate string, maxInterval, timeout time.Duration) (*TTSResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type out struct {
		r   *TTSResult
		err error
	}
	ch := make(chan out, 1+c.MaxHedges)
	launch := func() {
		go func() {
			r, err := c.generateOnce(ctx, texts, voice, resourceID, rate, maxInterval, timeout)
			ch <- out{r, err}
		}()
	}
	launch()
	launched, running := 1, 1
	var hedge <-chan time.Time
	if c.HedgeAfter > 0 && c.MaxHedges > 0 {
		hedge = time.After(c.HedgeAfter)
	}
	var lastErr error
	for {
		select {
		case o := <-ch:
			running--
			if o.err == nil {
				if launched > 1 {
					c.hedgeWins.Add(1)
				}
				return o.r, nil
			}
			var te *TaskError
			if errors.As(o.err, &te) && !te.Timeout { // deterministic failure: don't wait for others
				return o.r, o.err
			}
			lastErr = o.err
			if running == 0 {
				return nil, lastErr
			}
		case <-hedge:
			hedge = nil
			if launched <= c.MaxHedges {
				launch()
				launched++
				running++
				if launched <= c.MaxHedges {
					hedge = time.After(c.HedgeAfter)
				}
			}
		case <-ctx.Done():
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, &TaskError{Kind: "tts", Status: "processing", Timeout: true, TaskID: fmt.Sprintf("(%d attempts)", launched)}
		}
	}
}

// HedgeWins counts requests where a hedged duplicate was needed and succeeded.
func (c *Client) HedgeWins() int64 { return c.hedgeWins.Load() }

func (c *Client) generateOnce(ctx context.Context, texts []string, voice, resourceID, rate string, maxInterval, timeout time.Duration) (*TTSResult, error) {
	cr, err := c.CreateTTSTask(ctx, texts, voice, resourceID, rate)
	if err != nil {
		return nil, err
	}
	if len(cr.Data.Tasks) == 0 {
		return nil, &TaskError{Kind: "tts", Status: "missing", Msg: "no task returned: " + truncate(string(cr.Raw), 300)}
	}
	t := cr.Data.Tasks[0]
	qr, err := c.Poll(ctx, "tts", t.ID, t.Token, "", maxInterval, timeout)
	if err != nil {
		return nil, err
	}
	res, err := ParseTTSResult(qr)
	if res != nil {
		res.Token = t.Token
		res.Voice, res.ResourceID = ResolveVoice(voice, resourceID)
		res.Rate = rate
	}
	return res, err
}

// TranscribeBytes uploads media, creates an STT task and polls it (transcribe_file).
func (c *Client) TranscribeBytes(ctx context.Context, data []byte, o STTOptions, maxInterval, timeout time.Duration) (*UploadResult, *TaskResponse, error) {
	up, err := c.UploadBytes(ctx, data)
	if err != nil {
		return nil, nil, err
	}
	cr, err := c.CreateSTTTask(ctx, up.Vid, up.MD5, up.DurationMs, o)
	if err != nil {
		return up, nil, err
	}
	if len(cr.Data.Tasks) == 0 {
		return up, cr, &TaskError{Kind: "stt", Status: "missing", Msg: "no STT task returned"}
	}
	t := cr.Data.Tasks[0]
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if maxInterval <= 0 {
		maxInterval = 2 * time.Second
	}
	qr, err := c.Poll(ctx, "stt", t.ID, t.Token, "", maxInterval, timeout)
	if qr != nil && len(qr.Data.Tasks) > 0 && qr.Data.Tasks[0].Token == "" {
		qr.Data.Tasks[0].Token = t.Token
	}
	return up, qr, err
}

// ExtractSubtitles mirrors extract_subtitles.
func ExtractSubtitles(r *TaskResponse) (SubtitleResult, error) {
	if r == nil || len(r.Data.Tasks) == 0 {
		return SubtitleResult{Utterances: []Utterance{}}, nil
	}
	return ParseSubtitles(r.Data.Tasks[0].PayloadBytes())
}
