package capcut

import (
	"strings"
	"testing"
)

func TestCompactJSONMatchesPython(t *testing.T) {
	got := CompactJSON(Obj{{"a", "x<&>\"\\\n\t\x01é😀"}, {"b", []any{1, 2.5, 0.0, true, nil}}, {"c", Obj{}}})
	want := `{"a":"x<&>\"\\\n\t\u0001é😀","b":[1,2.5,0.0,true,null],"c":{}}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestCanonicalQuery(t *testing.T) {
	got := CanonicalQuery("https://h/top/v1?b=2&a=x+y&a=%2F&c=")
	if want := "a=%2F&a=x%20y&b=2&c="; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestResolveVoice(t *testing.T) {
	if vt, rid := ResolveVoice("nhỏ ngọt ngào", ""); vt != "BV421_vivn_streaming" || rid != "7252594014782755330" {
		t.Fatal(vt, rid)
	}
	if vt, rid := ResolveVoice("", ""); vt != DefaultVoice || rid == "" {
		t.Fatal(vt, rid)
	}
	if vt, rid := ResolveVoice("unknown", "9"); vt != "unknown" || rid != "9" {
		t.Fatal(vt, rid)
	}
}

func TestRSASignDecodable(t *testing.T) {
	s, err := MakeTTSPayloadSign("<speak/>", nil, "1", "2", DefaultEntropy().Rand)
	if err != nil || len(s) != 344 {
		t.Fatalf("sign len %d err %v", len(s), err)
	}
}

func TestSubtitlesSRT(t *testing.T) {
	r, err := ParseSubtitles([]byte(`{"utterances":[{"text":"Xin chào","start_time":280,"end_time":3720500,"words":[{"text":"Xin","start_time":280,"end_time":520}]}]}`))
	if err != nil || r.FullText != "Xin chào" || len(r.Utterances[0].Words) != 1 {
		t.Fatal(r, err)
	}
	if srt := r.SRT(); !strings.Contains(srt, "00:00:00,280 --> 01:02:00,500") {
		t.Fatal(srt)
	}
}
