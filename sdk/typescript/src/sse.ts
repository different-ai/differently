export interface SseMessage {
  event: string;
  data: string;
}

/** Parses a text/event-stream body. Cancels the stream if the consumer stops early. */
export async function* readSse(body: ReadableStream<Uint8Array>): AsyncGenerator<SseMessage, void, undefined> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let event = "";
  let data: string[] = [];
  try {
    for (;;) {
      const { value, done } = await reader.read();
      buffer += decoder.decode(value, { stream: !done });
      let start = 0;
      let end: number;
      while ((end = buffer.indexOf("\n", start)) >= 0) {
        let line = buffer.slice(start, end);
        start = end + 1;
        if (line.endsWith("\r")) line = line.slice(0, -1);
        if (line === "") {
          if (data.length > 0) yield { event: event || "message", data: data.join("\n") };
          event = "";
          data = [];
        } else if (!line.startsWith(":")) {
          const colon = line.indexOf(":");
          const field = colon < 0 ? line : line.slice(0, colon);
          let fieldValue = colon < 0 ? "" : line.slice(colon + 1);
          if (fieldValue.startsWith(" ")) fieldValue = fieldValue.slice(1);
          if (field === "event") event = fieldValue;
          else if (field === "data") data.push(fieldValue);
        }
      }
      buffer = buffer.slice(start);
      if (done) return;
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
}
