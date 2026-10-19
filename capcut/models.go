package capcut

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed Voice.json
var voiceJSON []byte

type VoiceInfo struct {
	VoiceType   string `json:"voice_type"`
	DisplayName string `json:"display_name"`
	ResourceID  string `json:"resource_id"`
	Lang        string `json:"lang"`
	Lan         string `json:"lan"`
	CapturedAt  string `json:"captured_at,omitempty"`
}

var catalog = func() []VoiceInfo {
	var v []VoiceInfo
	if err := json.Unmarshal(voiceJSON, &v); err != nil {
		panic(err)
	}
	return v
}()

// Voices returns the embedded catalog, optionally filtered by lang ("vi" or "vi-VN")
// and a case-insensitive substring query over name / voice_type / resource_id.
func Voices(lang, q string) []VoiceInfo {
	lang, q = strings.ToLower(strings.TrimSpace(lang)), strings.ToLower(strings.TrimSpace(q))
	out := make([]VoiceInfo, 0, len(catalog))
	for _, v := range catalog {
		if lang != "" && strings.ToLower(v.Lang) != lang && strings.ToLower(v.Lan) != lang {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(v.DisplayName), q) &&
			!strings.Contains(strings.ToLower(v.VoiceType), q) && !strings.Contains(v.ResourceID, q) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Langs returns language codes with counts, Vietnamese first then by count.
func Langs() []map[string]any {
	cnt := map[string]int{}
	for _, v := range catalog {
		cnt[v.Lang]++
	}
	keys := make([]string, 0, len(cnt))
	for k := range cnt {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == "vi-VN") != (keys[j] == "vi-VN") {
			return keys[i] == "vi-VN"
		}
		if cnt[keys[i]] != cnt[keys[j]] {
			return cnt[keys[i]] > cnt[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make([]map[string]any, len(keys))
	for i, k := range keys {
		out[i] = map[string]any{"lang": k, "count": cnt[k]}
	}
	return out
}

// ResolveVoice mirrors CapCutClient.resolve_voice.
func ResolveVoice(voice, resourceID string) (string, string) {
	target := voice
	if target == "" {
		target = DefaultVoice
	}
	tl := strings.ToLower(strings.TrimSpace(target))
	pick := func(v VoiceInfo) (string, string) {
		if resourceID != "" {
			return v.VoiceType, resourceID
		}
		return v.VoiceType, v.ResourceID
	}
	for _, v := range catalog {
		if strings.ToLower(v.VoiceType) == tl {
			return pick(v)
		}
	}
	for _, v := range catalog {
		if strings.ToLower(v.DisplayName) == tl || (resourceID != "" && v.ResourceID == resourceID) {
			return pick(v)
		}
	}
	if resourceID == "" {
		resourceID = DefaultResourceID
	}
	return target, resourceID
}

type UploadResult struct {
	Vid        string `json:"vid"`
	MD5        string `json:"md5"`
	LocalMD5   string `json:"local_md5"`
	DurationMs int    `json:"duration_ms"`
	Format     string `json:"format,omitempty"`
	Size       int64  `json:"size"`
	FileType   string `json:"file_type,omitempty"`
	StoreURI   string `json:"store_uri,omitempty"`
}

type Word struct {
	Text          string `json:"text"`
	StartTime     int    `json:"start_time"`
	EndTime       int    `json:"end_time"`
	BlankDuration int    `json:"blank_duration"`
}

type Utterance struct {
	Text      string `json:"text"`
	StartTime int    `json:"start_time"`
	EndTime   int    `json:"end_time"`
	Words     []Word `json:"words"`
}

type SubtitleResult struct {
	Utterances []Utterance `json:"utterances"`
	FullText   string      `json:"full_text"`
	Language   string      `json:"language,omitempty"`
	Model      string      `json:"model,omitempty"`
}

// ParseSubtitles mirrors SubtitleResult.from_payload (payload is the decoded task payload).
func ParseSubtitles(payload []byte) (SubtitleResult, error) {
	var p struct {
		Utterances []Utterance `json:"utterances"`
		Model      string      `json:"model"`
		Attribute  struct {
			Extra struct {
				Language string `json:"language"`
			} `json:"extra"`
		} `json:"attribute"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return SubtitleResult{}, fmt.Errorf("parse subtitle payload: %w", err)
	}
	r := SubtitleResult{Utterances: p.Utterances, Model: p.Model, Language: p.Attribute.Extra.Language}
	if r.Utterances == nil {
		r.Utterances = []Utterance{}
	}
	parts := make([]string, 0, len(r.Utterances))
	for i := range r.Utterances {
		if r.Utterances[i].Words == nil {
			r.Utterances[i].Words = []Word{}
		}
		if t := r.Utterances[i].Text; t != "" {
			parts = append(parts, t)
		}
	}
	r.FullText = strings.Join(parts, " ")
	return r, nil
}

func srtTime(ms int) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// SRT renders utterances as SubRip text.
func (s SubtitleResult) SRT() string {
	var b strings.Builder
	n := 0
	for _, u := range s.Utterances {
		if strings.TrimSpace(u.Text) == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", n, srtTime(u.StartTime), srtTime(u.EndTime), strings.TrimSpace(u.Text))
	}
	return b.String()
}
