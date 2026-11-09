package clientgen

const tsRealtimeRuntime = `
export interface Event<T> { id: string; event: string; data: T; retry?: number }
// EventStream retains the native response and supports one sequential reader.
// Breaking iteration or calling close cancels the body. There is no reconnect.
export class EventStream<T> implements AsyncIterable<Event<T>> {
 readonly response: Response;
 private readonly limit: number;
 private reader?: ReadableStreamDefaultReader<Uint8Array>;
 private started = false;
 private closed = false;
 constructor(response: Response, maxEventBytes = 1024 * 1024) {
  this.response = response;
  try {
   if (response.headers.get('Content-Type')?.split(';', 1)[0].trim().toLowerCase() !== 'text/event-stream') throw new Error('Expected text/event-stream response');
   this.limit = positiveLimit(maxEventBytes, 'maxEventBytes');
  } catch (error) { void response.body?.cancel().catch(() => {}); throw error; }
 }
 async close(): Promise<void> {
  if (this.closed) return; this.closed = true;
  if (this.reader) await this.reader.cancel(); else await this.response.body?.cancel();
 }
 async *[Symbol.asyncIterator](): AsyncGenerator<Event<T>> {
  if (this.started) throw new Error('EventStream has already been consumed'); this.started = true;
  if (this.closed || !this.response.body) return;
  const reader = this.response.body.getReader(); this.reader = reader;
  const decoder = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });
  let line: number[] = [], size = 0, skipLF = false, first = true;
  let data: string[] = [], event = '', id = '', retry: number | undefined;
  const processLine = (): Event<T> | undefined => {
   let text = decoder.decode(new Uint8Array(line)); line = [];
   if (first) { first = false; if (text.startsWith('\uFEFF')) text = text.slice(1); }
   if (text === '') {
    size = 0;
    const result = data.length ? { id, event: event || 'message', data: JSON.parse(data.join('\n')) as T, ...(retry === undefined ? {} : {retry}) } : undefined;
    data = []; event = ''; retry = undefined; return result;
   }
   if (text.startsWith(':')) return;
   const colon = text.indexOf(':'); const field = colon < 0 ? text : text.slice(0, colon);
   let value = colon < 0 ? '' : text.slice(colon + 1); if (value.startsWith(' ')) value = value.slice(1);
   switch (field) {
    case 'data': data.push(value); break;
    case 'event': event = value; break;
    case 'id': if (!value.includes('\0')) id = value; break;
    case 'retry': if (/^[0-9]+$/.test(value) && Number.isSafeInteger(Number(value))) retry = Number(value); break;
   }
  };
  try {
   for (;;) {
    const chunk = await reader.read(); if (this.closed) return;
    if (chunk.done) { if (size || line.length) throw new Error('Truncated event stream'); return; }
    for (const value of chunk.value) {
     if (this.closed) return;
     if (skipLF) { skipLF = false; if (value === 10) continue; }
     if (++size > this.limit) throw new Error('Event exceeds size limit');
     if (value === 10 || value === 13) { skipLF = value === 13; const result = processLine(); if (result) yield result; }
     else line.push(value);
    }
   }
  } finally {
   this.closed = true;
   try { await reader.cancel(); } finally { reader.releaseLock(); this.reader = undefined; }
  }
 }
}
function positiveLimit(value: number, name: string): number {
 if (!Number.isSafeInteger(value) || value <= 0) throw new Error(name + ' must be a positive safe integer'); return value;
}
// A native-compatible text WebSocket. Node transports may implement this shape.
export interface SocketConnection {
 readonly readyState: number; readonly bufferedAmount: number;
 send(data: string): void; close(code?: number, reason?: string): void;
 addEventListener(type: string, listener: EventListener): void;
 removeEventListener(type: string, listener: EventListener): void;
}
export type SocketFactory = (url: string, options: {headers: Headers; signal?: AbortSignal}) => SocketConnection;
// Socket exposes its native transport and one sequential message iterator.
export class Socket<I, O> implements AsyncIterable<O> {
 readonly raw: SocketConnection;
 private readonly limit: number;
 private readonly queueLimit: number;
 private queue: O[] = [];
 private wake?: () => void;
 private ended = false;
 private failure?: Error;
 private consuming = false;
 private opened = false;
 private resolveOpen!: () => void;
 private rejectOpen!: (error: Error) => void;
 readonly ready: Promise<void>;
 private readonly signal?: AbortSignal;
 constructor(raw: SocketConnection, limit: number, queueLimit: number, signal?: AbortSignal) {
  this.raw = raw; this.limit = limit; this.queueLimit = queueLimit; this.signal = signal;
  this.ready = new Promise<void>((resolve, reject) => { this.resolveOpen = resolve; this.rejectOpen = reject; });
  raw.addEventListener('open', this.onOpen); raw.addEventListener('message', this.onMessage);
  raw.addEventListener('error', this.onError); raw.addEventListener('close', this.onClose);
  signal?.addEventListener('abort', this.onAbort, {once: true});
  if (signal?.aborted) this.onAbort(); else if (raw.readyState === 1) this.onOpen(new globalThis.Event('open'));
  else if (raw.readyState > 1) this.finish(new Error('WebSocket closed before opening'));
 }
 private onOpen: EventListener = () => { if (this.ended) return; this.opened = true; this.resolveOpen(); };
 private onMessage: EventListener = event => {
  if (this.ended) return;
  try {
   const data: unknown = (event as MessageEvent<unknown>).data;
   if (typeof data !== 'string') throw new Error('Expected a WebSocket text message');
   if (new TextEncoder().encode(data).byteLength > this.limit) throw new Error('Socket message exceeds size limit');
   if (this.queue.length >= this.queueLimit) throw new Error('Socket message queue exceeds size limit');
   this.queue.push(JSON.parse(data) as O); this.wake?.(); this.wake = undefined;
  } catch (error) { this.finish(error instanceof Error ? error : new Error('Invalid socket message')); }
 };
 private onError: EventListener = () => { this.finish(new Error('WebSocket transport failed')); };
 private onClose: EventListener = event => {
  const code = (event as CloseEvent).code;
  this.finish(!this.opened ? new Error('WebSocket closed before opening') : code === 1000 || code === 1001 ? undefined : new Error('WebSocket closed unexpectedly (' + code + ')'));
 };
 private onAbort = (): void => { this.finish(new DOMException('WebSocket operation aborted', 'AbortError')); };
 private finish(error?: Error): void {
  if (this.ended) return; this.ended = true; this.failure = error;
  if (!this.opened) this.rejectOpen(error ?? new Error('WebSocket closed before opening'));
  if (error) this.queue = [];
  this.raw.removeEventListener('open', this.onOpen); this.raw.removeEventListener('message', this.onMessage);
  this.raw.removeEventListener('error', this.onError); this.raw.removeEventListener('close', this.onClose);
  this.signal?.removeEventListener('abort', this.onAbort);
  try { this.raw.close(1000, 'Client closed'); } catch { /* already closed */ }
  this.wake?.(); this.wake = undefined;
 }
 send(value: I): void {
  if (this.ended || this.raw.readyState !== 1) throw this.failure ?? new Error('WebSocket is not open');
  const data = JSON.stringify(value); if (data === undefined) throw new Error('Socket message is not JSON');
  const size = new TextEncoder().encode(data).byteLength;
  if (size > this.limit) throw new Error('Socket message exceeds size limit');
  if (this.raw.bufferedAmount + size > this.limit * this.queueLimit) throw new Error('Socket send buffer exceeds size limit');
  this.raw.send(data);
 }
 close(): void { this.finish(); }
 async *messages(): AsyncGenerator<O> {
  if (this.consuming) throw new Error('Socket messages already have a reader'); this.consuming = true;
  try {
   for (;;) {
    if (this.failure) throw this.failure;
    if (this.queue.length) { yield this.queue.shift()!; continue; }
    if (this.ended) return;
    await new Promise<void>(resolve => { this.wake = resolve; });
   }
  } finally { this.consuming = false; this.close(); }
 }
 [Symbol.asyncIterator](): AsyncGenerator<O> { return this.messages(); }
}
async function connectSocket<I, O>(url: string, headers: Headers, signal: AbortSignal | undefined, config: ClientOptions): Promise<Socket<I, O>> {
 const limit = positiveLimit(config.maxMessageBytes ?? 64 * 1024, 'maxMessageBytes');
 const queueLimit = positiveLimit(config.maxQueuedMessages ?? 32, 'maxQueuedMessages');
 if (!Number.isSafeInteger(limit * queueLimit)) throw new Error('Socket buffer limit exceeds supported range');
 if (signal?.aborted) throw new DOMException('WebSocket operation aborted', 'AbortError');
 let raw: SocketConnection;
 if (config.socketFactory) raw = config.socketFactory(url, {headers, signal});
 else {
  let hasHeaders = false; headers.forEach(() => { hasHeaders = true; });
  if (hasHeaders) throw new Error('Custom WebSocket headers require socketFactory; browser WebSocket cannot send them');
  if (typeof globalThis.WebSocket !== 'function') throw new Error('WebSocket is unavailable; configure socketFactory');
  raw = new globalThis.WebSocket(url);
 }
 const socket = new Socket<I, O>(raw, limit, queueLimit, signal); await socket.ready; return socket;
}
`
