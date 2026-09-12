package push

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"smspub/internal/model"
)

// sendFunc 单次发送；错误分类见 retryable。
type sendFunc func(title, body string) error

// statusError 远端返回非 2xx 状态码。
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// permError 配置类错误，重试无意义。
type permError struct{ err error }

func (e *permError) Error() string { return e.err.Error() }
func (e *permError) Unwrap() error { return e.err }

func permf(format string, args ...any) error { return &permError{fmt.Errorf(format, args...)} }

// retryable 判定是否值得重试：5xx/429 与传输错误重试；4xx 与配置错误不重试。
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	var pe *permError
	if errors.As(err, &pe) {
		return false
	}
	return true // 传输错误一律重试
}

// 固定服务地址（生产值严格按契约；测试替换为 httptest 地址）。
var (
	telegramAPI   = "https://api.telegram.org"
	serverChanAPI = "https://sctapi.ftqq.com"
	pushPlusURL   = "https://www.pushplus.plus/send"
)

// buildSender 按通道类型构造发送函数；未知类型或缺配置返回 permError。
func buildSender(ch model.Channel, c *http.Client) (sendFunc, error) {
	cfg := ch.Config
	get := func(k string) string { return strings.TrimSpace(cfg[k]) }
	switch ch.Type {
	case "Bark":
		u, err := httpURL(ensureHTTPS(get("url")))
		if err != nil {
			return nil, permf("Bark url 无效: %v", err)
		}
		return func(title, body string) error {
			return postJSON(c, u, titleBodyPayload{Title: title, Body: body})
		}, nil

	case "Telegram":
		token, chatID := get("botToken"), get("chatId")
		if token == "" || chatID == "" {
			return nil, permf("Telegram 需要 botToken 与 chatId")
		}
		u, err := httpURL(telegramAPI + "/bot" + token + "/sendMessage")
		if err != nil {
			return nil, permf("Telegram 地址无效: %v", err)
		}
		return func(title, body string) error {
			return postJSON(c, u, telegramPayload{ChatID: chatID, Text: title + "\n" + body})
		}, nil

	case "钉钉机器人":
		return buildDingTalk(get("webhook"), get("secret"), c)

	case "飞书机器人":
		return buildFeishu(get("webhook"), get("secret"), c)

	case "Server酱":
		key := get("sendKey")
		if key == "" {
			return nil, permf("Server酱需要 sendKey")
		}
		u, err := httpURL(serverChanAPI + "/" + key + ".send")
		if err != nil {
			return nil, permf("Server酱地址无效: %v", err)
		}
		return func(title, body string) error {
			form := url.Values{"title": {title}, "desp": {body}}.Encode()
			req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form))
			if err != nil {
				return &permError{err}
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return doHTTP(c, req)
		}, nil

	case "PushPlus":
		token := get("token")
		if token == "" {
			return nil, permf("PushPlus 需要 token")
		}
		return func(title, body string) error {
			return postJSON(c, pushPlusURL, pushPlusPayload{
				Token: token, Title: title, Content: body, Template: "txt",
			})
		}, nil

	case "Gotify":
		base, token := strings.TrimRight(get("url"), "/"), get("token")
		if base == "" || token == "" {
			return nil, permf("Gotify 需要 url 与 token")
		}
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		u, err := httpURL(base + "/message" + sep + "token=" + url.QueryEscape(token))
		if err != nil {
			return nil, permf("Gotify url 无效: %v", err)
		}
		return func(title, body string) error {
			return postJSON(c, u, gotifyPayload{Title: title, Message: body})
		}, nil

	case "SMTP 邮件":
		return smtpSender(cfg)

	case "POST JSON":
		u, err := httpURL(get("url"))
		if err != nil {
			return nil, permf("POST JSON url 无效: %v", err)
		}
		return func(title, body string) error {
			return postJSON(c, u, titleBodyPayload{Title: title, Body: body})
		}, nil

	case "GET 请求":
		tpl := get("url")
		if tpl == "" {
			return nil, permf("GET 请求需要 url")
		}
		if _, err := httpURL(tpl); err != nil {
			return nil, permf("GET 请求 url 无效: %v", err)
		}
		return func(title, body string) error {
			u := strings.ReplaceAll(tpl, "{title}", url.QueryEscape(title))
			u = strings.ReplaceAll(u, "{body}", url.QueryEscape(body))
			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return &permError{err}
			}
			return doHTTP(c, req)
		}, nil

	case "自定义模板":
		return buildCustom(cfg, c)

	default:
		return nil, permf("未知通道类型 %q", ch.Type)
	}
}

// buildDingTalk 钉钉机器人：加签参数拼在 webhook 上，JSON 文本消息。
func buildDingTalk(webhook, secret string, c *http.Client) (sendFunc, error) {
	if webhook == "" {
		return nil, permf("钉钉机器人需要 webhook")
	}
	base, err := httpURL(webhook)
	if err != nil {
		return nil, permf("钉钉 webhook 无效: %v", err)
	}
	bu, _ := url.Parse(base)
	return func(title, body string) error {
		u := *bu // 拷贝，避免跨尝试污染
		if secret != "" {
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10) // 钉钉加签用毫秒时间戳
			sep := "&"
			if u.RawQuery == "" {
				sep = "?"
			}
			u.RawQuery += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(dingSign(secret, ts))
		}
		p := dingPayload{MsgType: "text"}
		p.Text.Content = title + "\n" + body
		return postJSONCheck(c, u.String(), p, checkDingResp)
	}, nil
}

// buildFeishu 飞书机器人：timestamp/sign 放在 JSON body 中。
func buildFeishu(webhook, secret string, c *http.Client) (sendFunc, error) {
	if webhook == "" {
		return nil, permf("飞书机器人需要 webhook")
	}
	if _, err := httpURL(webhook); err != nil {
		return nil, permf("飞书 webhook 无效: %v", err)
	}
	return func(title, body string) error {
		p := feishuPayload{MsgType: "text"}
		p.Content.Text = title + "\n" + body
		if secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10) // 飞书加签用秒级时间戳
			p.Timestamp = ts
			p.Sign = feishuSign(secret, ts)
		}
		return postJSONCheck(c, webhook, p, checkFeishuResp)
	}, nil
}

// buildCustom 自定义模板通道：可自定义方法、请求头与 body 模板。
func buildCustom(cfg map[string]string, c *http.Client) (sendFunc, error) {
	u, err := httpURL(strings.TrimSpace(cfg["url"]))
	if err != nil {
		return nil, permf("自定义模板 url 无效: %v", err)
	}
	method := strings.ToUpper(strings.TrimSpace(cfg["method"]))
	if method == "" {
		method = http.MethodPost
	}
	tpl := cfg["body"]
	if tpl == "" {
		tpl = `{"title":"{title}","body":"{body}"}`
	}
	hdrs := map[string]string{}
	if h := strings.TrimSpace(cfg["headers"]); h != "" {
		if err := json.Unmarshal([]byte(h), &hdrs); err != nil {
			return nil, permf("自定义模板 headers 不是合法的 JSON 对象: %v", err)
		}
	}
	return func(title, body string) error {
		text := strings.ReplaceAll(tpl, "{title}", title)
		text = strings.ReplaceAll(text, "{body}", body)
		var rd io.Reader
		if method != http.MethodGet && method != http.MethodHead {
			rd = strings.NewReader(text)
		}
		req, err := http.NewRequest(method, u, rd)
		if err != nil {
			return &permError{err}
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		if rd != nil && text != "" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		return doHTTP(c, req)
	}, nil
}

// dingSign 钉钉加签：base64(hmac_sha256(key=secret, msg=timestamp+"\n"+secret))。
func dingSign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSign 飞书加签：base64(hmac_sha256(key=timestamp+"\n"+secret, msg=""))。
func feishuSign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// postJSON 序列化 payload 并 POST（Content-Type: application/json）。
func postJSON(c *http.Client, u string, payload any) error {
	return postJSONCheck(c, u, payload, nil)
}

// postJSONCheck 同 postJSON，check 非 nil 时对 2xx 响应 body 做业务码校验。
func postJSONCheck(c *http.Client, u string, payload any, check func([]byte) error) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return &permError{err}
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return &permError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	return doHTTPCheck(c, req, check)
}

// doHTTP 执行请求并归一化错误：非 2xx → statusError；网络失败原样返回（可重试）。
func doHTTP(c *http.Client, req *http.Request) error {
	return doHTTPCheck(c, req, nil)
}

// doHTTPCheck 同 doHTTP，check 非 nil 时在 2xx 后对限量读取的 body 做业务码校验
// （钉钉/飞书失败时仍返回 HTTP 200，需解码 body 判定），返回非 nil 即本次发送失败。
func doHTTPCheck(c *http.Client, req *http.Request, check func([]byte) error) error {
	resp, err := c.Do(req)
	if err != nil {
		return redactTransportErr(err) // *url.Error 文本含完整 URL，脱敏防 token 入日志
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{code: resp.StatusCode}
	}
	if check != nil {
		return check(b)
	}
	return nil
}

// checkDingResp 钉钉业务码校验：errcode 非 0 返回错误（普通 error，走既有重试）；
// body 非 JSON（解码失败）宽容处理，不算失败。
func checkDingResp(body []byte) error {
	var r struct {
		Errcode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Errcode == 0 {
		return nil
	}
	return fmt.Errorf("钉钉业务错误 errcode=%d: %s", r.Errcode, r.Errmsg)
}

// checkFeishuResp 飞书业务码校验：code 非 0 返回错误（普通 error，走既有重试）；
// body 非 JSON（解码失败）宽容处理，不算失败。
func checkFeishuResp(body []byte) error {
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Code == 0 {
		return nil
	}
	return fmt.Errorf("飞书业务错误 code=%d: %s", r.Code, r.Msg)
}

// redactURL 脱敏 URL：保留 scheme://host/path，丢弃 userinfo/query/fragment
// （钉钉 webhook、Gotify 的 token 在 query）。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(URL 无法解析，已隐藏)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// redactTransportErr 传输错误脱敏：*url.Error 的文本含完整 URL，而 path 亦可能内嵌
// 凭据（Telegram bot token 在 /bot<token>/ 路径），重建为仅含 scheme://host 的错误。
func redactTransportErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := "(URL 已隐藏)"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		host = u.Host
		if u.Scheme != "" {
			host = u.Scheme + "://" + u.Host
		}
	}
	return &url.Error{Op: ue.Op, URL: host, Err: ue.Err}
}

// httpURL 校验 scheme/host 齐全的合法 URL。
func httpURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("缺少 scheme/host: %q", redactURL(raw))
	}
	return raw, nil
}

// ensureHTTPS 无 scheme 的地址补 https://。
func ensureHTTPS(u string) string {
	if u != "" && !strings.Contains(u, "://") {
		return "https://" + u
	}
	return u
}

// ---- 各通道 JSON 载荷（字段名严格按各服务契约） ----

type titleBodyPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type telegramPayload struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

type dingPayload struct {
	MsgType string      `json:"msgtype"`
	Text    dingContent `json:"text"`
}

type dingContent struct {
	Content string `json:"content"`
}

type feishuPayload struct {
	Timestamp string        `json:"timestamp,omitempty"`
	Sign      string        `json:"sign,omitempty"`
	MsgType   string        `json:"msg_type"`
	Content   feishuContent `json:"content"`
}

type feishuContent struct {
	Text string `json:"text"`
}

type pushPlusPayload struct {
	Token    string `json:"token"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Template string `json:"template"`
}

type gotifyPayload struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}
