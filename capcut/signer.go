package capcut

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/big"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entropy holds the clock / randomness sources, injectable for parity tests.
type Entropy struct {
	Now  func() time.Time
	UUID func() string // canonical lowercase dashed UUID v4
	Rand io.Reader
}

func DefaultEntropy() Entropy {
	return Entropy{Now: time.Now, UUID: NewUUID4, Rand: rand.Reader}
}

// NewUUID4 returns a random RFC 4122 v4 UUID string.
func NewUUID4() string {
	var u [16]byte
	_, _ = io.ReadFull(rand.Reader, u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return FormatUUID(u[:])
}

func FormatUUID(u []byte) string {
	h := hex.EncodeToString(u)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func md5Hex(s []byte) string    { h := md5.Sum(s); return hex.EncodeToString(h[:]) }
func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// MakeXSSStub = md5(body).
func MakeXSSStub(body string) string { return md5Hex([]byte(body)) }

// MakeTraceID builds "00-<hex32>-<hex16>-01" from a uuid4.
func MakeTraceID(uuid string) string {
	seed := strings.ReplaceAll(uuid, "-", "")
	return "00-" + seed + "-" + seed[:16] + "-01"
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func EscapeXML(s string) string { return xmlEscaper.Replace(s) }

var (
	pubOnce sync.Once
	pubKey  *rsa.PublicKey
	pubErr  error
)

func ttsPublicKey() (*rsa.PublicKey, error) {
	pubOnce.Do(func() {
		blk, _ := pem.Decode([]byte(TTSSignPublicKeyPEM))
		if blk == nil {
			pubErr = errors.New("bad PEM")
			return
		}
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			pubErr = err
			return
		}
		var ok bool
		if pubKey, ok = k.(*rsa.PublicKey); !ok {
			pubErr = errors.New("not an RSA key")
		}
	})
	return pubKey, pubErr
}

// RSAEncryptPKCS1v15 replicates signer.rsa_encrypt_pkcs1v15 exactly, including how
// the non-zero padding is drawn from the random source (so it is reproducible).
func RSAEncryptPKCS1v15(msg []byte, rnd io.Reader) (string, error) {
	pk, err := ttsPublicKey()
	if err != nil {
		return "", err
	}
	keyLen := (pk.N.BitLen() + 7) / 8
	if len(msg) > keyLen-11 {
		return "", errors.New("message too long for RSA PKCS#1 v1.5 padding")
	}
	psLen := keyLen - len(msg) - 3
	ps := make([]byte, 0, psLen)
	for len(ps) < psLen {
		chunk := make([]byte, psLen-len(ps))
		if _, err := io.ReadFull(rnd, chunk); err != nil {
			return "", err
		}
		for _, b := range chunk {
			if b != 0 {
				ps = append(ps, b)
			}
		}
	}
	em := make([]byte, 0, keyLen)
	em = append(em, 0x00, 0x02)
	em = append(em, ps[:psLen]...)
	em = append(em, 0x00)
	em = append(em, msg...)
	c := new(big.Int).Exp(new(big.Int).SetBytes(em), big.NewInt(int64(pk.E)), pk.N)
	out := c.FillBytes(make([]byte, keyLen))
	return base64.StdEncoding.EncodeToString(out), nil
}

// MakeTTSPayloadSign = RSA(appid:..&did:..&creditDisable:false&ssml:md5[&extraInfo:..]).
func MakeTTSPayloadSign(ssml string, extraInfo *string, deviceID, appID string, rnd io.Reader) (string, error) {
	in := fmt.Sprintf("appid:%s&did:%s&creditDisable:false&ssml:%s", appID, deviceID, md5Hex([]byte(ssml)))
	if extraInfo != nil {
		in += "&extraInfo:" + *extraInfo
	}
	return RSAEncryptPKCS1v15([]byte(in), rnd)
}

// MakeSignHeader = md5("9e2c|<last 7 of path>|3|appvr|device-time|tdid|11ac").
func MakeSignHeader(rawURL, appvr, deviceTime, tdid string) string {
	path := rawURL
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	tail := path
	if len(tail) > 7 {
		tail = tail[len(tail)-7:]
	}
	return md5Hex([]byte("9e2c|" + tail + "|3|" + appvr + "|" + deviceTime + "|" + tdid + "|11ac"))
}

// AWS4SigningKey derives the SigV4 key for VOD.
func AWS4SigningKey(secret, dateStamp string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	k = hmacSHA256(k, []byte(VODRegion))
	k = hmacSHA256(k, []byte(VODService))
	return hmacSHA256(k, []byte("aws4_request"))
}

// pyQuote == urllib.parse.quote(s, safe="-_.~") on UTF-8 bytes.
func pyQuote(s string) string {
	const hx = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hx[c>>4])
			b.WriteByte(hx[c&15])
		}
	}
	return b.String()
}

// CanonicalQuery mirrors signer.canonical_query (parse_qsl keep_blank_values, sorted pairs).
func CanonicalQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(u.RawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		pairs = append(pairs, pair{k, v})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = pyQuote(p.k) + "=" + pyQuote(p.v)
	}
	return strings.Join(out, "&")
}

// AWS4Authorization mirrors signer.aws4_authorization.
func AWS4Authorization(method, rawURL string, body []byte, akid, secret, token, amzDate string) string {
	dateStamp := amzDate[:8]
	scope := dateStamp + "/" + VODRegion + "/" + VODService + "/aws4_request"
	signed := "x-amz-date;x-amz-security-token"
	canonHeaders := "x-amz-date:" + amzDate + "\nx-amz-security-token:" + token + "\n"
	path := ""
	if u, err := url.Parse(rawURL); err == nil {
		path = u.EscapedPath()
	}
	canon := strings.Join([]string{method, path, CanonicalQuery(rawURL), canonHeaders, signed, sha256Hex(body)}, "\n")
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canon))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(AWS4SigningKey(secret, dateStamp), []byte(sts)))
	return "AWS4-HMAC-SHA256 Credential=" + akid + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

// VODTimes returns (amz_date, http_date) in UTC.
func VODTimes(now time.Time) (string, string) {
	u := now.UTC()
	return u.Format("20060102T150405Z"), u.Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

func CRC32Hex(b []byte) string { return fmt.Sprintf("%08x", crc32.ChecksumIEEE(b)) }

// CommonQuery mirrors signer.common_query.
func CommonQuery(d DeviceConfig, babi Obj, includeRegion bool) Query {
	q := Query{
		{"app_name", d.AppName}, {"device_type", d.DeviceType}, {"os_version", d.OSVersion},
		{"channel", d.Channel}, {"version_name", d.VersionName}, {"device_brand", d.DeviceBrand},
		{"device_id", d.DeviceID}, {"iid", d.IID}, {"version_code", d.VersionCode},
		{"device_platform", d.DevicePlatform}, {"aid", d.Aid},
	}
	if includeRegion {
		q = append(q, KV{"region", d.Region})
	}
	if babi != nil {
		q = append(q, KV{"babi_param", CompactJSON(babi)})
	}
	return q
}

// Header is an ordered header list (order kept for dumps; irrelevant on the wire).
type Header []KV

func (h Header) Get(k string) string {
	for _, kv := range h {
		if strings.EqualFold(kv.K, k) {
			return kv.V.(string)
		}
	}
	return ""
}

func (h Header) Map() map[string]string {
	m := make(map[string]string, len(h))
	for _, kv := range h {
		m[kv.K] = kv.V.(string)
	}
	return m
}

// BaseHeaders mirrors signer.base_headers.
func BaseHeaders(d DeviceConfig, body string, appid bool, e Entropy) Header {
	now := strconv.FormatInt(e.Now().Unix(), 10)
	h := Header{
		{"content-type", "application/json"}, {"appvr", d.Appvr}, {"ch", d.Channel},
		{"device-time", now}, {"lan", d.Lan}, {"loc", d.Loc}, {"pf", d.Pf}, {"sign-ver", "1"},
		{"tdid", d.Tdid}, {"x-ss-stub", MakeXSSStub(body)}, {"x-ss-dp", d.Aid}, {"x-khronos", now},
		{"x-tt-trace-id", MakeTraceID(e.UUID())}, {"user-agent", userAgent},
		{"accept-encoding", "gzip, deflate"}, {"store-country-code", strings.ToLower(d.Loc)},
		{"store-country-code-src", "did"}, {"is-dispatch-us-ttp", "0"}, {"is-app-region-us-ttp", "0"},
	}
	if appid {
		h = append(h, KV{"app-sdk-version", d.Appvr}, KV{"appid", d.Aid})
	}
	return h
}
