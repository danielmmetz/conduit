/* global self, clients */
"use strict";

const PREFIX = "/_/";

self.addEventListener("install", (event) => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(clients.claim());
});

const streams = new Map();

class DownloadStream {
  constructor(name, size, filetype) {
    this.name = name;
    this.size = size;
    this.filetype = filetype;
    this.offset = 0;
    this.controller = null;
    this.requestHandled = false;
    this.streamHandled = false;
    const streamSelf = this;
    this.stream = new ReadableStream({
      start(controller) {
        streamSelf.controller = controller;
      },
      cancel(reason) {
        console.warn("conduit download stream cancelled", reason);
      },
    });
  }
}

function waitForMetadata(id) {
  return new Promise((resolve, reject) => {
    streams.set(id, { resolve, reject, pending: true });
  });
}

function signalMetadataReady(id, s) {
  const pending = streams.get(id);
  if (pending && pending.resolve) {
    pending.resolve(s);
  }
}

self.addEventListener("message", (e) => {
  const msg = e.data;
  if (!msg || typeof msg.id !== "string") {
    return;
  }
  const id = msg.id;

  switch (msg.type) {
    case "metadata": {
      const s = new DownloadStream(
        msg.name,
        msg.size,
        msg.filetype || "application/octet-stream"
      );
      signalMetadataReady(id, s);
      streams.set(id, s);
      return;
    }
    case "data": {
      const s = streams.get(id);
      if (!(s instanceof DownloadStream) || !s.controller) {
        console.warn("conduit sw: data for unknown or uninitialized transfer", id);
        return;
      }
      if (msg.offset !== s.offset) {
        console.warn("conduit sw: out-of-order chunk", id);
        streams.delete(id);
        try {
          s.controller.error(new Error("out of order"));
        } catch (_) {}
        return;
      }
      s.controller.enqueue(new Uint8Array(msg.data));
      s.offset += msg.data.byteLength;
      return;
    }
    case "end": {
      const s = streams.get(id);
      if (!(s instanceof DownloadStream) || !s.controller) {
        return;
      }
      try {
        s.controller.close();
      } catch (_) {}
      if (s.requestHandled) {
        streams.delete(id);
      } else {
        s.streamHandled = true;
      }
      return;
    }
    case "error": {
      const s = streams.get(id);
      if (s instanceof DownloadStream && s.controller) {
        try {
          s.controller.error(msg.error || "transfer error");
        } catch (_) {}
      }
      streams.delete(id);
      return;
    }
    default:
      return;
  }
});

function encodeFilename(filename) {
  return encodeURIComponent(filename)
    .replace(/'/g, "%27")
    .replace(/\(/g, "%28")
    .replace(/\)/g, "%29")
    .replace(/\*/g, "%2A");
}

async function streamDownload(id) {
  let s = streams.get(id);
  if (!(s instanceof DownloadStream)) {
    s = await waitForMetadata(id);
  }

  if (s.streamHandled) {
    streams.delete(id);
  } else {
    s.requestHandled = true;
  }

  return new Response(s.stream, {
    headers: {
      "Content-Type": s.filetype,
      "Content-Length": String(s.size),
      "Content-Disposition": `attachment; filename*=UTF-8''${encodeFilename(s.name)}`,
    },
  });
}

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith(PREFIX) && e.request.method === "GET") {
    const id = url.pathname.slice(PREFIX.length);
    if (!id) {
      e.respondWith(new Response("missing id", { status: 400 }));
      return;
    }
    e.respondWith(
      streamDownload(id).catch(() => new Response("download failed", { status: 500 }))
    );
    return;
  }
  e.respondWith(fetch(e.request));
});
