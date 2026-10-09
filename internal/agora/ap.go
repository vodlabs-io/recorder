package agora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// apHosts are the access points the Web SDK asks for a gateway, tried in
// order: Agora's own domain first, then the SD-RTN mirror.
var apHosts = []string{
	"https://webrtc2-ap-web-1.agora.io",
	"https://webrtc2-2.ap.sd-rtn.com",
}

// gatewayFlag marks the access-point entry that lists WebSocket gateways (the
// other entry lists TURN relays).
const gatewayFlag = 4096

type edge struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// apGrant is the access point's answer: where to connect and the signed
// ticket the gateway admits the client on. It is echoed back verbatim in the
// join, so its fields keep the access point's own names.
type apGrant struct {
	enterTs int64
	opid    int64
	Cert    string            `json:"cert"`
	Cid     int64             `json:"cid"`
	Cname   string            `json:"cname"`
	Code    int               `json:"code"`
	Detail  map[string]string `json:"detail"`
	Edges   []edge            `json:"edges_services"`
	Flag    int64             `json:"flag"`
	UID     uint32            `json:"uid"`
}

// joinResponse is the grant in the shape the join message carries it.
func (g apGrant) joinResponse() map[string]any {
	return map[string]any{
		"code": 0, "server_ts": g.enterTs, "uid": g.UID, "cid": g.Cid, "cname": g.Cname,
		"detail": g.Detail, "flag": g.Flag, "opid": g.opid, "cert": g.Cert, "ticket": g.Cert,
	}
}

// lookupGateway asks the access points for a gateway serving ch.
func lookupGateway(ctx context.Context, client *http.Client, ch Channel, sid string) (apGrant, error) {
	var errs []error
	for _, host := range apHosts {
		g, err := queryAP(ctx, client, host, ch, sid)
		if err == nil {
			return g, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", host, err))
	}
	return apGrant{}, fmt.Errorf("access point: %w", errors.Join(errs...))
}

func queryAP(ctx context.Context, client *http.Client, host string, ch Channel, sid string) (apGrant, error) {
	opid := time.Now().UnixNano() % 1e11
	body, err := json.Marshal(map[string]any{
		"appid": ch.AppID, "client_ts": time.Now().UnixMilli(), "opid": opid, "sid": sid,
		"request_bodies": []any{map[string]any{
			"uri": 22,
			"buffer": map[string]any{
				"cname":       ch.Name,
				"detail":      map[string]string{"11": "CN,GLOBAL", "17": "2", "22": "CN,GLOBAL"},
				"key":         ch.Token,
				"service_ids": []int{11, 26},
				"uid":         ch.UID,
			},
		}},
	})
	if err != nil {
		return apGrant{}, err
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	if err := mw.WriteField("request", string(body)); err != nil {
		return apGrant{}, err
	}
	if err := mw.Close(); err != nil {
		return apGrant{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/v2/transpond/webrtc?v=2", &form)
	if err != nil {
		return apGrant{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return apGrant{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return apGrant{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return apGrant{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var reply struct {
		EnterTs      int64 `json:"enter_ts"`
		Opid         int64 `json:"opid"`
		ResponseBody []struct {
			Buffer apGrant `json:"buffer"`
		} `json:"response_body"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return apGrant{}, fmt.Errorf("decode: %w", err)
	}
	for _, r := range reply.ResponseBody {
		g := r.Buffer
		if g.Flag != gatewayFlag {
			continue
		}
		if g.Code != 0 {
			return apGrant{}, fmt.Errorf("refused with code %d", g.Code)
		}
		if len(g.Edges) == 0 {
			return apGrant{}, errors.New("no gateway offered")
		}
		g.enterTs, g.opid = reply.EnterTs, reply.Opid
		return g, nil
	}
	return apGrant{}, errors.New("no gateway entry in reply")
}
