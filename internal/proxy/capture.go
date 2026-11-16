package proxy

import (
	"errors"
	"net/http"
	"time"

	"mrp/internal/capture"
	"mrp/internal/log"
)

// harTimeLayout HAR 1.2 时间戳格式。
const harTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// SetCapture 注入抓包记录器，nil 表示关闭抓包；须在开始接受请求前调用一次。
func (p *Proxy) SetCapture(recorder *capture.Recorder) {
	p.capture.Store(recorder)
}

// Close 关闭抓包记录器补全 HAR 文件尾部，未开启时为空操作。
func (p *Proxy) Close() error {
	if recorder := p.capture.Load(); recorder != nil {
		return recorder.Close()
	}
	return nil
}

// recordCapture 在响应回传后组装一条 HAR entry 并追加；记录失败或达上限时告警一次并停用，不影响转发。
func (p *Proxy) recordCapture(captured *capturedRequest, request *http.Request, response *captureWriter) {
	recorder := p.capture.Load()
	if recorder == nil {
		return
	}
	elapsed := float64(time.Since(captured.start).Milliseconds())
	bodySize := captured.bodySize
	if captured.bodyTail != nil {
		bodySize += captured.bodyTail.count
	}
	var postData *capture.PostData
	if len(captured.bodyPrefix) > 0 {
		text, enc, _ := capture.DecodeContent(captured.bodyPrefix, request.Header.Get("Content-Encoding"), request.Header.Get("Content-Type"))
		postData = &capture.PostData{
			MimeType:  request.Header.Get("Content-Type"),
			Text:      text,
			Encoding:  enc,
			Truncated: captured.bodyTruncated,
		}
	}
	responseText, responseEnc, responseSize := capture.DecodeContent(response.prefix, response.header.Get("Content-Encoding"), capture.ContentTypeOf(response.header))
	entry := capture.Entry{
		StartedDateTime: captured.start.UTC().Format(harTimeLayout),
		Time:            elapsed,
		ServerName:      captured.domain,
		ClientAddress:   request.RemoteAddr,
		Request: capture.Request{
			Method:      request.Method,
			URL:         absoluteTarget(captured.protocol, captured.port, captured.domain, request.URL),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     capture.HeadersFrom(request.Header),
			QueryString: []capture.Query{},
			PostData:    postData,
			HeadersSize: -1, // 未计算请求头字节数，按 HAR 规范以 -1 表示未知，避免误导性 0
			BodySize:    bodySize,
		},
		Response: capture.Response{
			Status:      response.Status,
			StatusText:  http.StatusText(response.Status),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     capture.HeadersFrom(response.header),
			Content: capture.Content{
				Size:      responseSize,
				MimeType:  capture.ContentTypeOf(response.header),
				Text:      responseText,
				Encoding:  responseEnc,
				Truncated: response.truncated,
			},
			HeadersSize: -1, // 未计算响应头字节数，按 HAR 规范以 -1 表示未知，避免误导性 0
			BodySize:    response.total,
		},
		Timings: capture.Timings{Wait: elapsed},
	}
	switch err := recorder.Record(entry); {
	case errors.Is(err, capture.ErrLimitReached):
		log.Warn("capture stopped path=%s: file size limit reached", recorder.Path())
	case err != nil:
		log.Warn("capture stopped path=%s err=%v", recorder.Path(), err)
	}
}
