//go:build integration

package clientgen_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"trest.local/trest"
)

type streamRequest struct {
	Mode *string `query:"mode"`
}
type streamPayload struct {
	Name string `json:"name"`
}

func TestGeneratedEventStreamsRoundTrip(t *testing.T) {
	app, err := trest.New(trest.Get("/events", func(context.Context, streamRequest) (trest.Events[streamPayload], error) {
		return trest.EventStream(func(_ context.Context, writer *trest.JSONEventWriter[streamPayload]) error {
			if err := writer.Heartbeat(); err != nil {
				return err
			}
			if err := writer.Write(trest.JSONEvent[streamPayload]{ID: "one", Event: "update", Data: streamPayload{Name: "café"}, Retry: 250 * time.Millisecond}); err != nil {
				return err
			}
			return writer.Send("two", "", streamPayload{Name: "done"})
		}), nil
	}).Name("events"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("mode") {
		case "wait":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"name\":\"waiting\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "reject":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = fmt.Fprint(w, `{"code":"unauthorized","message":"Authentication required"}`)
		default:
			app.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("TREST_STREAM_URL", server.URL)
	exerciseGeneratedClients(t, document, streamGoTest, streamTSTest)
}

const streamGoTest = `package generated
import("context";"errors";"io";"net/http";"os";"strings";"testing")
type streamRoundTripper func(*http.Request)(*http.Response,error)
func(f streamRoundTripper)RoundTrip(r *http.Request)(*http.Response,error){return f(r)}
type oneByteReader struct{io.Reader}
func(r oneByteReader)Read(p []byte)(int,error){if len(p)>1{p=p[:1]};return r.Reader.Read(p)}
func TestEvents(t *testing.T){
 client,err:=NewClient(os.Getenv("TREST_STREAM_URL"),nil);if err!=nil{t.Fatal(err)}
 stream,err:=client.Events(context.Background(),EventsInput{});if err!=nil{t.Fatal(err)}
 if stream.Response.StatusCode!=200||!stream.Next(){t.Fatalf("first event: %v",stream.Err())}
 first:=stream.Event();if first.ID!="one"||first.Event!="update"||first.Data.Name!="café"||first.RetryMillis==nil||*first.RetryMillis!=250{t.Fatalf("first event changed: %#v",first)}
 if !stream.Next()||stream.Event().ID!="two"||stream.Event().Event!="message"||stream.Event().Data.Name!="done"{t.Fatalf("second event: %#v %v",stream.Event(),stream.Err())}
 if stream.Next()||stream.Err()!=nil{t.Fatalf("stream end: %v",stream.Err())};_=stream.Close()
 mode:="wait";stream,err=client.Events(context.Background(),EventsInput{Query:EventsQuery{Mode:&mode}});if err!=nil||!stream.Next(){t.Fatalf("close stream: %v",err)};if err=stream.Close();err!=nil{t.Fatal(err)}
 ctx,cancel:=context.WithCancel(context.Background());stream,err=client.Events(ctx,EventsInput{Query:EventsQuery{Mode:&mode}});if err!=nil||!stream.Next(){cancel();t.Fatalf("cancel stream: %v",err)};cancel();if stream.Next()||!errors.Is(stream.Err(),context.Canceled){t.Fatalf("cancellation not preserved: %v",stream.Err())}
 mode="reject";_,err=client.Events(context.Background(),EventsInput{Query:EventsQuery{Mode:&mode}});var api *APIError;if !errors.As(err,&api)||api.Status!=401{t.Fatalf("API error changed: %v",err)}
}
func TestEventParser(t *testing.T){
 t.Run("explicit close discards buffered events",func(t *testing.T){
  client,_:=NewClient("http://example.test",&http.Client{Transport:streamRoundTripper(func(*http.Request)(*http.Response,error){return &http.Response{StatusCode:200,Header:http.Header{"Content-Type":[]string{"text/event-stream"}},Body:io.NopCloser(strings.NewReader("data: {\"name\":\"one\"}\n\ndata: {\"name\":\"two\"}\n\n"))},nil})})
  stream,err:=client.Events(context.Background(),EventsInput{});if err!=nil{t.Fatal(err)};if !stream.Next(){t.Fatal(stream.Err())};_=stream.Close();if stream.Next()||stream.Err()!=nil{t.Fatalf("closed stream kept reading: %v",stream.Err())}
 })
 for _,tc:=range []struct{name,body string;limit int64;count int;fail bool}{
  {name:"chunked CR LF UTF8 and multiline",body:"\ufeff: heartbeat\r\n\r\nid: carried\revent: update\rretry: 25\rdata: {\"name\":\rdata: \"café\"}\r\rdata: {\"name\":\"next\"}\n\n",count:2},
  {name:"comments",body:": heartbeat\n\n"},
  {name:"ignored fields",body:"retry: invalid\nid: bad\x00id\nunknown: ignored\ndata: {\"name\":\"ok\"}\n\n",count:1},
  {name:"invalid JSON",body:"data: {broken}\n\n",fail:true},
  {name:"truncated frame",body:"data: {\"name\":\"x\"}\n",fail:true},
  {name:"truncated line",body:"data: {\"name\":\"x\"}",fail:true},
  {name:"invalid UTF8",body:"data: \xff\n\n",fail:true},
  {name:"oversize comment",body:":"+strings.Repeat("x",64)+"\n\n",limit:32,fail:true},
 }{t.Run(tc.name,func(t *testing.T){
  client,_:=NewClient("http://example.test",&http.Client{Transport:streamRoundTripper(func(*http.Request)(*http.Response,error){return &http.Response{StatusCode:200,Header:http.Header{"Content-Type":[]string{"text/event-stream"}},Body:io.NopCloser(oneByteReader{strings.NewReader(tc.body)})},nil})})
  if tc.limit>0{client.MaxEventBytes=tc.limit}
  stream,err:=client.Events(context.Background(),EventsInput{});if err!=nil{t.Fatal(err)}
  count:=0;for stream.Next(){count++;if tc.name=="chunked CR LF UTF8 and multiline"{event:=stream.Event();if event.ID!="carried"{t.Fatal("last event ID lost")};if count==1&&(event.Data.Name!="café"||event.RetryMillis==nil||*event.RetryMillis!=25){t.Fatalf("chunk boundaries changed event: %#v",event)};if count==2&&event.RetryMillis!=nil{t.Fatal("retry leaked into next event")}}}
  if count!=tc.count||(stream.Err()!=nil)!=tc.fail{t.Fatalf("count=%d err=%v",count,stream.Err())}
 })}
}
`

const streamTSTest = `import {Client,EventStream} from './client';
declare const process:{env:Record<string,string|undefined>;exitCode:number};
async function main(){
 const client=new Client(process.env.TREST_STREAM_URL!);
 const stream=await client.events();const events=[];
 for await(const event of stream)events.push(event);
 if(stream.response.status!==200||events.length!==2||events[0].id!=='one'||events[0].event!=='update'||events[0].data.name!=='café'||events[0].retry!==250||events[1].id!=='two'||events[1].event!=='message')throw new Error('typed stream changed');
 const early=await client.events({query:{mode:'wait'}});for await(const event of early){if(event.data.name!=='waiting')throw new Error('waiting event changed');break;}
 const controller=new AbortController();const canceled=await client.events({query:{mode:'wait'}},{signal:controller.signal});const iterator=canceled[Symbol.asyncIterator]();await iterator.next();controller.abort();let failed=false;try{await iterator.next();}catch{failed=true;}if(!failed)throw new Error('abort did not reject');
 failed=false;try{await client.events({query:{mode:'reject'}});}catch(error){failed=!!error&&typeof error==='object'&&'status' in error&&error.status===401;}if(!failed)throw new Error('API error changed');
 const buffered=new EventStream<{name:string}>(new Response('data: {"name":"one"}\n\ndata: {"name":"two"}\n\n',{headers:{'Content-Type':'text/event-stream'}}));
 const bufferedIterator=buffered[Symbol.asyncIterator]();await bufferedIterator.next();await buffered.close();if(!(await bufferedIterator.next()).done)throw new Error('closed stream yielded buffered event');
 const cases=[
  {name:'chunked',body:'\uFEFF: heartbeat\r\n\r\nid: carried\revent: update\rretry: 25\rdata: {"name":\rdata: "café"}\r\rdata: {"name":"next"}\n\n',count:2},
  {name:'comments',body:': heartbeat\n\n',count:0},
  {name:'ignored',body:'retry: invalid\nid: bad\0id\nunknown: ignored\ndata: {"name":"ok"}\n\n',count:1},
  {name:'invalid JSON',body:'data: {broken}\n\n',count:0,fail:true},
  {name:'truncated frame',body:'data: {"name":"x"}\n',count:0,fail:true},
  {name:'truncated line',body:'data: {"name":"x"}',count:0,fail:true},
  {name:'oversize',body:':'+ 'x'.repeat(64)+'\n\n',count:0,fail:true,limit:32},
 ];
 for(const test of cases){
  const bytes=new TextEncoder().encode(test.body);let offset=0;let canceled=false;
  const body=new ReadableStream<Uint8Array>({pull(controller){if(offset<bytes.length)controller.enqueue(bytes.slice(offset,++offset));else controller.close();},cancel(){canceled=true;}});
  const stream=new EventStream<{name:string}>(new Response(body,{headers:{'Content-Type':'text/event-stream'}}),test.limit);
  const received=[];let failed=false;try{for await(const event of stream)received.push(event);}catch{failed=true;}
  if(received.length!==test.count||failed!==!!test.fail)throw new Error(test.name+' parser mismatch');
  if(test.name==='chunked'&&(received[0].data.name!=='café'||received[0].retry!==25||received[1].id!=='carried'||received[1].retry!==undefined))throw new Error('chunk framing changed');
  if(body.locked)throw new Error('parser retained the body lock');
  if(test.name==='oversize'&&!canceled)throw new Error('oversize parser did not cancel response');
 }
 let invalid=false;try{for await(const event of new EventStream(new Response(new Uint8Array([100,97,116,97,58,32,255,10,10]),{headers:{'Content-Type':'text/event-stream'}})))void event;}catch{invalid=true;}if(!invalid)throw new Error('invalid UTF8 accepted');
}
main().catch(error=>{console.error(error);process.exitCode=1;});
`
