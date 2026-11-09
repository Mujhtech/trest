package clientgen

const goRealtimeRuntime = `
// Event is one JSON server-sent event. RetryMillis is optional reconnection
// metadata; this client never reconnects or replays a request automatically.
type Event[T any] struct { ID string; Event string; Data T; RetryMillis *int64 }
// EventStream exposes the native response. Call Next sequentially and Close
// when stopping early. The request context controls reads and cancellation.
type EventStream[T any] struct {
 Response *http.Response
 reader *bufio.Reader
 limit int64
 size int64
 skipLF bool
 first bool
 done bool
 lastID string
 current Event[T]
 err error
 closed atomic.Bool
 closeOnce sync.Once
 closeErr error
}
func newEventStream[T any](response *http.Response,limit int64)(*EventStream[T],error){
 media,_,err:=mime.ParseMediaType(response.Header.Get("Content-Type"))
 if err!=nil||media!="text/event-stream"{response.Body.Close();return nil,fmt.Errorf("expected text/event-stream response")}
 if limit<=0||limit>1<<40{response.Body.Close();return nil,fmt.Errorf("MaxEventBytes must be positive and at most 1 TiB")}
 return &EventStream[T]{Response:response,reader:bufio.NewReader(response.Body),limit:limit,first:true},nil
}
func(s *EventStream[T])Close()error{if s==nil{return nil};s.closed.Store(true);s.closeOnce.Do(func(){s.closeErr=s.Response.Body.Close()});return s.closeErr}
func(s *EventStream[T])Event()Event[T]{return s.current}
func(s *EventStream[T])Err()error{return s.err}
func(s *EventStream[T])line()(string,error){
 var line []byte
 for {
  value,err:=s.reader.ReadByte();if err!=nil{if err==io.EOF&&len(line)>0{return "",io.ErrUnexpectedEOF};return "",err}
  if s.skipLF{s.skipLF=false;if value=='\n'{continue}}
  s.size++;if s.size>s.limit{return "",fmt.Errorf("event exceeds size limit")}
  if value=='\r'{s.skipLF=true;break};if value=='\n'{break};line=append(line,value)
 }
 if !utf8.Valid(line){return "",fmt.Errorf("event contains invalid UTF-8")}
 value:=string(line);if s.first{s.first=false;value=strings.TrimPrefix(value,"\ufeff")};return value,nil
}
func(s *EventStream[T])Next()bool{
 if s==nil||s.done||s.closed.Load(){return false}
 var data strings.Builder;hasData:=false;event:="";var retry *int64
 for {
  line,err:=s.line()
  if s.closed.Load(){s.done=true;return false}
  if err!=nil{
   s.done=true
   if err==io.EOF {if s.size!=0{s.err=io.ErrUnexpectedEOF}}else{s.err=err}
   _=s.Close();return false
  }
  if line=="" {
   s.size=0
   if !hasData{event="";retry=nil;continue}
   var value T
   if err=json.Unmarshal([]byte(data.String()),&value);err!=nil{s.err=fmt.Errorf("decode event: %w",err);s.done=true;_=s.Close();return false}
   if event==""{event="message"}
   s.current=Event[T]{ID:s.lastID,Event:event,Data:value,RetryMillis:retry};return true
  }
  if strings.HasPrefix(line,":"){continue}
  field,value,found:=strings.Cut(line,":");if !found{value=""};value=strings.TrimPrefix(value," ")
  switch field {
  case "data":if hasData{data.WriteByte('\n')};data.WriteString(value);hasData=true
  case "event":event=value
  case "id":if !strings.ContainsRune(value,0){s.lastID=value}
  case "retry":
   digits:=value!="";for _,c:=range value{if c<'0'||c>'9'{digits=false;break}}
   if digits{if ms,e:=strconv.ParseInt(value,10,64);e==nil{retry=&ms}}
  }
 }
}
// SocketTransport is the native JSON text-message boundary. An adapter must
// support cancellation and bound frames while reading, before allocating them.
type SocketTransport interface { Read(context.Context)([]byte,error); Write(context.Context,[]byte)error; Close()error }
type SocketDialer func(context.Context,string,http.Header)(SocketTransport,error)
// Socket preserves access to Transport. Reads are sequential; the injected
// transport defines concurrent read/write and shutdown behavior.
type Socket[I,O any] struct {Transport SocketTransport;maxMessageBytes int64;closed atomic.Bool;closeOnce sync.Once;closeErr error}
func(s *Socket[I,O])Send(ctx context.Context,value I)error{
 if s.closed.Load(){return fmt.Errorf("socket is closed")};if err:=ctx.Err();err!=nil{return err};data,err:=json.Marshal(value);if err!=nil{return fmt.Errorf("encode socket message: %w",err)}
 if int64(len(data))>s.maxMessageBytes{return fmt.Errorf("socket message exceeds size limit")};return s.Transport.Write(ctx,data)
}
func(s *Socket[I,O])Receive(ctx context.Context)(O,error){
 var value O;if s.closed.Load(){return value,fmt.Errorf("socket is closed")};if err:=ctx.Err();err!=nil{return value,err};data,err:=s.Transport.Read(ctx);if err!=nil{return value,err}
 if int64(len(data))>s.maxMessageBytes{_=s.Close();return value,fmt.Errorf("socket message exceeds size limit")}
 if !utf8.Valid(data){_=s.Close();return value,fmt.Errorf("socket message contains invalid UTF-8")}
 if err=json.Unmarshal(data,&value);err!=nil{_=s.Close();return value,fmt.Errorf("decode socket message: %w",err)};return value,nil
}
func(s *Socket[I,O])Close()error{if s==nil{return nil};s.closed.Store(true);s.closeOnce.Do(func(){s.closeErr=s.Transport.Close()});return s.closeErr}
func(c *Client)connectSocket(ctx context.Context,path string,query url.Values,headers http.Header)(SocketTransport,error){
 if c==nil||c.SocketDialer==nil{return nil,fmt.Errorf("SocketDialer is required for typed WebSocket operations")}
 if c.MaxMessageBytes<=0||c.MaxMessageBytes>1<<40{return nil,fmt.Errorf("MaxMessageBytes must be positive and at most 1 TiB")}
 if err:=ctx.Err();err!=nil{return nil,err}
 target:=c.baseURL+path;if len(query)>0{target+="?"+query.Encode()};parsed,err:=url.Parse(target);if err!=nil{return nil,err};if parsed.Scheme=="https"{parsed.Scheme="wss"}else{parsed.Scheme="ws"}
 all:=c.Headers.Clone();if all==nil{all=make(http.Header)};for key,values:=range headers{all[key]=append([]string(nil),values...)}
 transport,err:=c.SocketDialer(ctx,parsed.String(),all);if err!=nil{return nil,err};if transport==nil{return nil,fmt.Errorf("SocketDialer returned a nil transport")};return transport,nil
}
`
