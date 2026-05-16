// MockEventSource — minimal stand-in for the browser EventSource so
// useTodayStream can be exercised under jsdom. Tests stub via
//   vi.stubGlobal("EventSource", MockEventSource)
// then call MockEventSource.last().emit("card.appended", payload).

type Listener = (ev: MessageEvent) => void;

export class MockEventSource {
  static instances: MockEventSource[] = [];

  url: string;
  closed = false;
  onerror: ((this: EventSource, ev: Event) => unknown) | null = null;

  private listeners = new Map<string, Listener[]>();

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: Listener) {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  removeEventListener(type: string, listener: Listener) {
    const list = this.listeners.get(type);
    if (!list) return;
    this.listeners.set(
      type,
      list.filter((l) => l !== listener),
    );
  }

  close() {
    this.closed = true;
  }

  // Test-side helper: dispatch a named event with stringified data.
  emit(type: string, data: unknown) {
    const ev = new MessageEvent(type, { data: JSON.stringify(data) });
    const list = this.listeners.get(type) ?? [];
    list.forEach((l) => l(ev));
  }

  // Test-side helper: trigger the error handler so reconnect logic runs.
  fail() {
    if (this.onerror) this.onerror.call(this as unknown as EventSource, new Event("error"));
  }

  static last(): MockEventSource {
    const last = MockEventSource.instances[MockEventSource.instances.length - 1];
    if (!last) throw new Error("MockEventSource: no instance has been constructed yet");
    return last;
  }

  static reset() {
    MockEventSource.instances = [];
  }
}
