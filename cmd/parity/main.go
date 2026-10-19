// Command parity dumps deterministic signed requests from the Go port, using the
// same patched clock/uuid/RNG as parity/py_dump.py, for byte-for-byte comparison.
package main

import (
	"encoding/json"
	"math/big"
	"os"
	"time"

	"github.com/Huyvux12/RobinCap/capcut"
)

const fixed = 1792390000

type stream struct{ n int }

func (s *stream) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(((s.n+i)*37 + 11) % 256)
	}
	s.n += len(p)
	return len(p), nil
}

func main() {
	mul, _ := new(big.Int).SetString("0123456789ABCDEF0FEDCBA987654321", 16)
	mod := new(big.Int).Lsh(big.NewInt(1), 128)
	cnt := int64(0)
	ent := capcut.Entropy{
		Now: func() time.Time { return time.Unix(fixed, 0) },
		UUID: func() string {
			cnt++
			v := new(big.Int).Mod(new(big.Int).Mul(big.NewInt(cnt), mul), mod)
			return capcut.FormatUUID(v.FillBytes(make([]byte, 16)))
		},
		Rand: &stream{},
	}
	type item struct {
		Name    string            `json:"name"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	var out []item
	add := func(name string, r *capcut.Request, err error) {
		if err != nil {
			panic(err)
		}
		out = append(out, item{name, r.URL, r.Headers.Map(), r.Body})
	}
	c := capcut.NewClient(capcut.DefaultDevice())
	c.Entropy = ent
	r, err := c.BuildTTSNewRequest([]string{"Xin chào các bạn.", "Tom & Jerry <\"q\"> 'x'\ttab\nnl 😀 \u2028 \x01 \\ /"}, "Nhỏ Ngọt Ngào", "", "1.2")
	add("tts_multi", r, err)
	r, err = c.BuildTTSNewRequest([]string{"hello"}, "", "", "1.0")
	add("tts_default", r, err)
	r, err = c.BuildTTSNewRequest([]string{"x"}, "foo_voice", "123", "0.8")
	add("tts_unknown", r, err)
	add("stt_new", c.BuildSTTNewRequest("v10639g5000", "a98dba1c36b6", 4008, capcut.STTOptions{Language: "vi-VN", TranslationLanguage: "en-US", UseTranslation: true}), nil)
	add("stt_new_default", c.BuildSTTNewRequest("vid2", "md52", 0, capcut.STTOptions{}), nil)
	add("query_tts", c.BuildQueryRequest("tid_1", "tok&en=1", "tts", "bind"), nil)
	add("query_stt", c.BuildQueryRequest("tid_2", "tok2", "stt", ""), nil)
	d, _ := capcut.DeviceFromJSON([]byte(`{"device_id":"111","iid":"222","tdid":"333","loc":"US","region":"US","lan":"en-US"}`))
	c2 := capcut.NewClient(d)
	c2.Entropy = ent
	r, err = c2.BuildTTSNewRequest([]string{"Custom device"}, "BV421_vivn_streaming", "", "1.0")
	add("tts_custom_device", r, err)
	add("upload_sign", c.BuildUploadSignRequest(), nil)
	creds := capcut.ExportCreds("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "STS+tok/en==")
	apply := "https://vod.example.com/top/v1?Action=ApplyUploadInner&SpaceName=space+a&UseQuic=false&Version=2020-11-19&device_platform=win"
	out = append(out, item{"vod_apply", apply, c.VODSignedHeaders("GET", apply, nil, creds).Map(), ""})
	cb := capcut.CompactJSON(capcut.Obj{{K: "Functions", V: []any{capcut.Obj{{K: "Input", V: capcut.Obj{{K: "SnapshotTime", V: 0.0}}}, {K: "Name", V: "Snapshot"}}}}, {K: "SessionKey", V: "sk=="}})
	commit := "https://vod.example.com/top/v1?Action=CommitUploadInner&SpaceName=s&Version=2020-11-19&device_platform=win"
	out = append(out, item{"vod_commit", commit, c.VODSignedHeaders("POST", commit, []byte(cb), creds).Map(), cb})
	out = append(out, item{"upload_bin", "", c.UploadBinaryHeaders("authXYZ", "deadbeef").Map(), ""})
	out = append(out, item{"misc", "", map[string]string{"crc32": capcut.CRC32Hex([]byte("hello world")),
		"sign":   capcut.MakeSignHeader("https://x/lv/v1/common_task/new?a=1", "8.7.0", "1792390000", "t"),
		"escape": capcut.EscapeXML(`<a href="x">'&'</a>`)}, ""})
	e := json.NewEncoder(os.Stdout)
	e.SetEscapeHTML(false)
	e.SetIndent("", " ")
	_ = e.Encode(out)
}
