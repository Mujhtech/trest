package trest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"
)

// Reply is a native HTTP response for files, HTML, redirects, or streaming.
// Return normal Go values for JSON responses with generated client types. A Reply
// route is HTTP-only and cannot be exposed as a persisted job or JSON tool.
type Reply struct {
	status int
	header http.Header
	body   []byte
	serve  func(http.ResponseWriter, *http.Request) error
	err    error
}

// Text returns UTF-8 plain text. A handler returning string also sends plain text.
func Text(value string) Reply { return Bytes([]byte(value), "text/plain; charset=utf-8") }

// HTML returns application-produced HTML. It does not escape untrusted strings;
// use html/template when rendering user data.
func HTML(value string) Reply { return Bytes([]byte(value), "text/html; charset=utf-8") }

// Bytes returns a copied response body with its explicit media type.
func Bytes(body []byte, contentType string) Reply {
	return Reply{status: 200, header: http.Header{"Content-Type": []string{contentType}}, body: append([]byte(nil), body...)}
}

// NoContent returns HTTP 204 with no body.
func NoContent() Reply { return Reply{status: 204} }

// Redirect returns a redirect response. Invalid codes/locations fail before writing.
func Redirect(location string, status int) Reply {
	return Reply{status: status, header: http.Header{"Location": []string{location}}, serve: func(w http.ResponseWriter, r *http.Request) error {
		if status != 301 && status != 302 && status != 303 && status != 307 && status != 308 {
			return errors.New("trest: invalid redirect status")
		}
		if strings.ContainsAny(location, "\r\n") {
			return errors.New("trest: invalid redirect location")
		}
		if _, err := url.Parse(location); err != nil {
			return err
		}
		http.Redirect(w, r, location, status)
		return nil
	}}
}

// WithHeader returns a copy with an additional response header.
func (r Reply) WithHeader(name, value string) Reply {
	r.header = r.header.Clone()
	if r.header == nil {
		r.header = make(http.Header)
	}
	r.header.Set(name, value)
	return r
}

// WithStatus changes a buffered response's status. Streaming, file, and redirect
// callbacks own their protocol statuses and reject changes before writing.
func (r Reply) WithStatus(status int) Reply {
	if r.serve != nil && status != r.status {
		r.err = errors.New("trest: callback response status is chosen by its constructor")
	}
	r.status = status
	return r
}

// File serves one file from an explicit filesystem, with no directory listing.
// Filesystem paths follow fs.ValidPath and must not include a leading slash.
func File(files fs.FS, name string) Reply {
	return Reply{status: 200, serve: func(w http.ResponseWriter, r *http.Request) error {
		if files == nil || !fs.ValidPath(name) || name == "." {
			return errors.New("trest: invalid file path")
		}
		f, err := files.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		stat, err := f.Stat()
		if err != nil {
			return err
		}
		if stat.IsDir() {
			return errors.New("trest: directory responses are disabled")
		}
		if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if seeker, ok := f.(io.ReadSeeker); ok {
			http.ServeContent(w, r, path.Base(name), stat.ModTime(), seeker)
			return nil
		}
		w.Header().Set("Content-Length", fmt.Sprint(stat.Size()))
		w.WriteHeader(200)
		if r.Method == "HEAD" {
			return nil
		}
		_, err = io.Copy(w, f)
		return err
	}}
}

// Stream sends chunks as the callback writes them. Each write is flushed and has
// a ten-second write deadline. The callback must honor cancellation; errors after
// headers are sent are recorded by AfterResponse, not appended as a JSON body.
func Stream(contentType string, send func(context.Context, io.Writer) error) Reply {
	return Reply{status: 200, header: http.Header{"Content-Type": []string{contentType}, "Cache-Control": []string{"no-cache"}}, serve: func(w http.ResponseWriter, r *http.Request) error {
		if send == nil {
			return errors.New("trest: nil stream callback")
		}
		if r.Method == "HEAD" {
			w.WriteHeader(200)
			return nil
		}
		return send(r.Context(), &streamWriter{ctx: r.Context(), writer: w})
	}}
}

type streamWriter struct {
	ctx    context.Context
	writer http.ResponseWriter
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	controller := http.NewResponseController(w.writer)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	n, err := w.writer.Write(p)
	if err == nil {
		err = controller.Flush()
	}
	return n, err
}

// Message is one server-sent event. Data is text; use EventWriter.JSON for values.
type Message struct {
	ID, Event, Data string
	Retry           time.Duration
}

// EventWriter writes bounded, correctly framed server-sent events.
type EventWriter struct{ writer io.Writer }

// Send emits a single event, rejecting invalid metadata and frames over 1 MiB.
func (w *EventWriter) Send(message Message) error {
	if w == nil || w.writer == nil {
		return errors.New("trest: event writer unavailable")
	}
	if strings.ContainsAny(message.ID, "\r\n\x00") || strings.ContainsAny(message.Event, "\r\n") || message.Retry < 0 {
		return errors.New("trest: invalid event metadata")
	}
	if len(message.Data)+len(message.ID)+len(message.Event) > 1<<20 {
		return errors.New("trest: event exceeds size limit")
	}
	var b strings.Builder
	if message.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", message.ID)
	}
	if message.Event != "" {
		fmt.Fprintf(&b, "event: %s\n", message.Event)
	}
	if message.Retry > 0 {
		fmt.Fprintf(&b, "retry: %d\n", message.Retry.Milliseconds())
	}
	remaining := strings.ReplaceAll(strings.ReplaceAll(message.Data, "\r\n", "\n"), "\r", "\n")
	for {
		line, rest, more := strings.Cut(remaining, "\n")
		if b.Len()+len(line)+8 > 1<<20 {
			return errors.New("trest: event frame exceeds size limit")
		}
		fmt.Fprintf(&b, "data: %s\n", line)
		if !more {
			break
		}
		remaining = rest
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w.writer, b.String())
	return err
}

// Heartbeat sends an SSE comment, which keeps an idle connection active without
// creating an application event. The write obeys stream cancellation/deadlines.
func (w *EventWriter) Heartbeat() error {
	if w == nil || w.writer == nil {
		return errors.New("trest: event writer unavailable")
	}
	_, err := io.WriteString(w.writer, ": keep-alive\n\n")
	return err
}

// JSON encodes a value into one event's data field.
func (w *EventWriter) JSON(id, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return w.Send(Message{ID: id, Event: event, Data: string(data)})
}

// SSE returns a server-sent event stream. Document it with Produces("text/event-stream").
func SSE(send func(context.Context, *EventWriter) error) Reply {
	return Stream("text/event-stream", func(ctx context.Context, w io.Writer) error {
		if send == nil {
			return errors.New("trest: nil event callback")
		}
		return send(ctx, &EventWriter{writer: w})
	})
}

func writeOutput(w http.ResponseWriter, r *http.Request, info RouteInfo, value any, maxBytes int64) error {
	if events, ok := value.(typedEventStream); ok {
		value = events.eventReply(info.streamValidator)
	}
	typedOutput := false
	if typed, ok := value.(typedResponse); ok {
		typedOutput = true
		index, body, headers, err := selectResponse(typed, info.Responses)
		if err != nil {
			return err
		}
		selected := info.Responses[index]
		info.Status = selected.Status
		info.ContentType = selected.ContentType
		value = body
		for name, values := range headers {
			w.Header()[name] = append([]string(nil), values...)
		}
	}
	if reply, ok := value.(Reply); ok && !typedOutput {
		if reply.err != nil {
			return reply.err
		}
		if reply.status < 200 || reply.status > 399 {
			return errors.New("trest: invalid response status")
		}
		if int64(len(reply.body)) > maxBytes {
			return errors.New("trest: response exceeds size limit")
		}
		for name, values := range reply.header {
			if strings.ContainsAny(name, "\r\n") {
				return errors.New("trest: invalid response header")
			}
			for _, v := range values {
				if strings.ContainsAny(v, "\r\n") {
					return errors.New("trest: invalid response header")
				}
			}
			w.Header()[name] = append([]string(nil), values...)
		}
		if reply.serve != nil {
			return reply.serve(w, r)
		}
		w.WriteHeader(reply.status)
		if r.Method == "HEAD" || reply.status == 204 || reply.status == 304 {
			return nil
		}
		_, err := w.Write(reply.body)
		return err
	}
	if info.Status == 204 || info.Status == 304 {
		w.WriteHeader(info.Status)
		return nil
	}
	if info.ContentType == "text/plain" {
		// Keep the validated underlying string, including for named string types.
		// StringWriter-capable transports can send it without a temporary []byte.
		text := reflect.ValueOf(value).String()
		if int64(len(text)) > maxBytes {
			return errors.New("trest: response exceeds size limit")
		}
		w.Header().Set("Content-Type", info.ContentType+"; charset=utf-8")
		w.WriteHeader(info.Status)
		if r.Method == "HEAD" {
			return nil
		}
		_, err := io.WriteString(w, text)
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return errors.New("trest: response exceeds size limit")
	}
	w.Header().Set("Content-Type", info.ContentType+"; charset=utf-8")
	w.WriteHeader(info.Status)
	if r.Method != "HEAD" {
		_, err = w.Write(data)
	}
	return err
}
