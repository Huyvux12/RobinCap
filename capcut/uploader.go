package capcut

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type uploadCreds struct {
	Domain          string `json:"domain"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	SpaceName       string `json:"space_name"`
}

// BuildUploadSignRequest mirrors VODUploader._upload_sign_request.
func (c *Client) BuildUploadSignRequest() *Request {
	body := CompactJSON(Obj{{"biz", "cc_pc_text_recognize"}, {"key_version", "v5"}})
	return c.sign("/lv/v1/upload_sign", CommonQuery(c.Device, nil, false), body, true)
}

// VODSignedHeaders mirrors VODUploader._vod_signed_headers.
func (c *Client) VODSignedHeaders(method, rawURL string, body []byte, cr uploadCreds) Header {
	now := c.Entropy.Now()
	amz, httpDate := VODTimes(now)
	return Header{
		{"Authorization", AWS4Authorization(method, rawURL, body, cr.AccessKeyID, cr.SecretAccessKey, cr.SessionToken, amz)},
		{"Date", httpDate}, {"User-Agent", fmt.Sprintf("BDFileUpload(%d)", now.UnixMilli())},
		{"X-Amz-Date", amz}, {"X-Amz-Expires", "31536000"}, {"X-Amz-Security-Token", cr.SessionToken},
		{"accept-encoding", "identity"}, {"store-country-code", strings.ToLower(c.Device.Loc)},
		{"store-country-code-src", "did"}, {"is-dispatch-us-ttp", "0"}, {"is-app-region-us-ttp", "0"},
		{"tdid", c.Device.Tdid}, {"pf", c.Device.Pf},
	}
}

func (c *Client) uploadBinaryHeaders(auth, crc string) Header {
	now := c.Entropy.Now()
	_, httpDate := VODTimes(now)
	h := Header{
		{"Authorization", auth}, {"Date", httpDate}, {"User-Agent", fmt.Sprintf("BDFileUpload(%d)", now.UnixMilli())},
		{"accept-encoding", "identity"}, {"store-country-code", strings.ToLower(c.Device.Loc)},
		{"store-country-code-src", "did"}, {"is-dispatch-us-ttp", "0"}, {"is-app-region-us-ttp", "0"},
		{"tdid", c.Device.Tdid}, {"pf", c.Device.Pf},
	}
	if crc != "" {
		h = append(h, KV{"X-Upload-Content-CRC32", crc})
	}
	return h
}

func (c *Client) raw(ctx context.Context, label, method, rawURL string, hdr func() Header, body []byte, out any) error {
	b, st, err := c.do(ctx, label, func() (*Request, error) {
		return &Request{Method: method, URL: rawURL, Headers: hdr(), Body: string(body)}, nil
	}, http.Header{})
	if err != nil {
		return err
	}
	return checkedJSON(label, b, st, out, false)
}

// UploadBytes uploads media to the CapCut VOD space (upload_sign → ApplyUploadInner →
// transfer → finish → CommitUploadInner), mirroring VODUploader.upload_file.
func (c *Client) UploadBytes(ctx context.Context, data []byte) (*UploadResult, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("upload: empty file")
	}
	localMD5 := md5Hex(data)
	crc := CRC32Hex(data)

	// 1. upload_sign
	var sign struct {
		Data uploadCreds `json:"data"`
	}
	b, st, err := c.do(ctx, "upload_sign", func() (*Request, error) { return c.BuildUploadSignRequest(), nil }, nil)
	if err != nil {
		return nil, err
	}
	if err := checkedJSON("upload_sign", b, st, &sign, true); err != nil {
		return nil, err
	}
	cr := sign.Data
	for k, v := range map[string]string{"domain": cr.Domain, "access_key_id": cr.AccessKeyID,
		"secret_access_key": cr.SecretAccessKey, "session_token": cr.SessionToken, "space_name": cr.SpaceName} {
		if v == "" {
			return nil, fmt.Errorf("upload_sign missing required field %q: %s", k, truncate(string(b), 300))
		}
	}

	// 2. ApplyUploadInner
	applyURL := "https://" + cr.Domain + "/top/v1?" + Query{{"Action", "ApplyUploadInner"}, {"SpaceName", cr.SpaceName},
		{"UseQuic", "false"}, {"Version", "2020-11-19"}, {"device_platform", "win"}}.Encode()
	var apply struct {
		Result struct {
			InnerUploadAddress struct {
				UploadNodes []struct {
					UploadHost string   `json:"UploadHost"`
					Vid        string   `json:"Vid"`
					Vids       []string `json:"Vids"`
					SessionKey string   `json:"SessionKey"`
					StoreInfos []struct {
						StoreURI string `json:"StoreUri"`
						UploadID string `json:"UploadID"`
						Auth     string `json:"Auth"`
					} `json:"StoreInfos"`
				} `json:"UploadNodes"`
			} `json:"InnerUploadAddress"`
		} `json:"Result"`
	}
	if err := c.raw(ctx, "ApplyUploadInner", "GET", applyURL, func() Header { return c.VODSignedHeaders("GET", applyURL, nil, cr) }, nil, &apply); err != nil {
		return nil, err
	}
	nodes := apply.Result.InnerUploadAddress.UploadNodes
	if len(nodes) == 0 || len(nodes[0].StoreInfos) == 0 {
		return nil, fmt.Errorf("ApplyUploadInner: no upload node")
	}
	node, store := nodes[0], nodes[0].StoreInfos[0]
	vid := node.Vid
	if vid == "" && len(node.Vids) > 0 {
		vid = node.Vids[0]
	}

	// 3. transfer (single part)
	base := "https://" + node.UploadHost + "/upload/v1/" + store.StoreURI + "?"
	transferURL := base + Query{{"uploadid", store.UploadID}, {"part_number", "0"}, {"phase", "transfer"}}.Encode()
	var tmp map[string]any
	if err := c.raw(ctx, "upload transfer", "POST", transferURL, func() Header { return c.uploadBinaryHeaders(store.Auth, crc) }, data, &tmp); err != nil {
		return nil, err
	}
	// 4. finish
	finishURL := base + Query{{"uploadmode", "part"}, {"phase", "finish"}, {"uploadid", store.UploadID}}.Encode()
	if err := c.raw(ctx, "upload finish", "POST", finishURL, func() Header { return c.uploadBinaryHeaders(store.Auth, "") }, []byte("0:"+crc), &tmp); err != nil {
		return nil, err
	}
	// 5. CommitUploadInner
	commitURL := "https://" + cr.Domain + "/top/v1?" + Query{{"Action", "CommitUploadInner"}, {"SpaceName", cr.SpaceName},
		{"Version", "2020-11-19"}, {"device_platform", "win"}}.Encode()
	commitBody := []byte(CompactJSON(Obj{{"Functions", []any{Obj{{"Input", Obj{{"SnapshotTime", 0.0}}}, {"Name", "Snapshot"}}}},
		{"SessionKey", node.SessionKey}}))
	var commit struct {
		Result struct {
			Results []struct {
				Vid       string `json:"Vid"`
				VideoMeta struct {
					Duration json.Number `json:"Duration"`
					Md5      string      `json:"Md5"`
					Format   string      `json:"Format"`
					Size     json.Number `json:"Size"`
					FileType string      `json:"FileType"`
					URI      string      `json:"Uri"`
				} `json:"VideoMeta"`
			} `json:"Results"`
		} `json:"Result"`
	}
	if err := c.raw(ctx, "CommitUploadInner", "POST", commitURL, func() Header { return c.VODSignedHeaders("POST", commitURL, commitBody, cr) }, commitBody, &commit); err != nil {
		return nil, err
	}
	if len(commit.Result.Results) == 0 {
		return nil, fmt.Errorf("CommitUploadInner: empty Results")
	}
	r := commit.Result.Results[0]
	m := r.VideoMeta
	out := &UploadResult{Vid: r.Vid, MD5: m.Md5, LocalMD5: localMD5, Format: m.Format, FileType: m.FileType, StoreURI: m.URI, Size: int64(len(data))}
	if f, err := strconv.ParseFloat(string(m.Duration), 64); err == nil {
		out.DurationMs = int(f * 1000)
	}
	if n, err := m.Size.Int64(); err == nil && n > 0 {
		out.Size = n
	}
	if out.Vid == "" {
		out.Vid = vid
	}
	if out.MD5 == "" {
		out.MD5 = localMD5
	}
	if out.StoreURI == "" {
		out.StoreURI = store.StoreURI
	}
	return out, nil
}

// ExportCreds builds VOD credentials (used by the parity harness).
func ExportCreds(akid, secret, token string) uploadCreds {
	return uploadCreds{AccessKeyID: akid, SecretAccessKey: secret, SessionToken: token}
}

// UploadBinaryHeaders is exported for the parity harness.
func (c *Client) UploadBinaryHeaders(auth, crc string) Header {
	return c.uploadBinaryHeaders(auth, crc)
}
