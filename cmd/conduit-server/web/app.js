/* global Go */

(function () {
  "use strict";

  const $ = (id) => document.getElementById(id);

  // Hosted rendezvous service. When the page is served from this origin the
  // Server field is hidden and the displayed CLI command omits --server, since
  // the conduit binary's own default points here.
  const DEFAULT_SERVER_URL = "https://conduit.danielmmetz.com";

  function defaultServerUrl() {
    return window.location.origin;
  }

  function isDefaultOrigin() {
    return window.location.origin === DEFAULT_SERVER_URL;
  }

  function setStatus(el, text, kind) {
    el.textContent = text || "";
    el.classList.remove("ok", "err");
    if (kind === "ok") {
      el.classList.add("ok");
    }
    if (kind === "err") {
      el.classList.add("err");
    }
  }

  // setConnectedStatus renders "Connected." plus a route pill (direct/relayed).
  // route comes from the WASM onPaired callback; "unknown" is rendered without
  // a pill so we don't show a misleading badge on edge cases (ICE not yet
  // converged, getStats unavailable, etc.).
  function setConnectedStatus(el, route) {
    el.replaceChildren();
    el.classList.remove("err");
    el.classList.add("ok");
    el.appendChild(document.createTextNode("Connected."));
    if (route === "direct" || route === "relayed") {
      const pill = document.createElement("span");
      pill.className = "route-pill " + route;
      pill.textContent = route;
      el.appendChild(pill);
    }
  }

  // formatBytes renders a binary-prefixed byte count using KB/MB/GB labels.
  function formatBytes(n) {
    if (n == null || n < 0) return "";
    const unit = 1024;
    if (n < unit) return n + " B";
    const units = ["KB", "MB", "GB", "TB", "PB"];
    let div = unit;
    let exp = 0;
    while (n / div >= unit && exp < units.length - 1) {
      div *= unit;
      exp++;
    }
    return (n / div).toFixed(1) + " " + units[exp];
  }

  // formatDuration renders seconds as a compact ETA: "<1s", "Ns", "MmSSs",
  // "Hh", "HhMMm". Mirrors the CLI's humanDuration so progress lines look
  // the same in both clients.
  function formatDuration(seconds) {
    if (seconds < 1) return "<1s";
    const s = Math.round(seconds);
    if (s < 60) return s + "s";
    const m = Math.floor(s / 60);
    const sr = s % 60;
    if (m < 60) {
      if (sr === 0) return m + "m";
      return m + "m" + String(sr).padStart(2, "0") + "s";
    }
    const h = Math.floor(m / 60);
    const mr = m % 60;
    if (mr === 0) return h + "h";
    return h + "h" + String(mr).padStart(2, "0") + "m";
  }

  // makeRowProgress returns an onProgress(done, total) callback that
  // updates the row's bytes/status cells with rate + ETA. inFlightLabel is
  // the status text rendered until the rate stabilises ("sending" /
  // "receiving"). Calls are throttled to ~10/s so a fast in-memory
  // transfer doesn't flood the DOM, but the final 100% update is always
  // emitted so the row settles.
  function makeRowProgress(row, inFlightLabel) {
    const startedAt = Date.now();
    let lastUpdate = 0;
    return (done, total) => {
      const now = Date.now();
      const final = total > 0 && done >= total;
      if (!final && now - lastUpdate < 100) return;
      lastUpdate = now;
      if (total > 0) {
        row.bytes.textContent =
          formatBytes(done) + " / " + formatBytes(total);
      } else {
        row.bytes.textContent = formatBytes(done);
      }
      const elapsed = (now - startedAt) / 1000;
      // 250ms warm-up matches the CLI: avoids the first sample rendering an
      // inflated rate dominated by setup overhead.
      if (done > 0 && elapsed >= 0.25) {
        const rate = done / elapsed;
        if (rate >= 1) {
          let s = formatBytes(Math.floor(rate)) + "/s";
          if (total > done) {
            s += " · ETA " + formatDuration((total - done) / rate);
          }
          row.status.textContent = s;
          return;
        }
      }
      row.status.textContent = inFlightLabel;
    };
  }

  function isTextPayload(kind, mime) {
    if (kind === "text") return true;
    if (typeof mime === "string" && mime.toLowerCase().startsWith("text/")) {
      return true;
    }
    return false;
  }

  function decodeUtf8(bytes) {
    try {
      return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    } catch (e) {
      return "";
    }
  }

  function triggerBlobDownload(blob, filename) {
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename || "conduit-received.bin";
    a.style.display = "none";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
  }

  function fileBasename(name) {
    if (!name || typeof name !== "string") return "";
    const parts = name.replace(/\\/g, "/").split("/");
    const base = parts.pop();
    return base || "";
  }

  function prefersServiceWorkerDownload() {
    if (!("serviceWorker" in navigator)) return false;
    if (!window.isSecureContext) return false;
    if (
      /Safari/.test(navigator.userAgent) &&
      !/Chrome|Chromium/.test(navigator.userAgent)
    ) {
      return false;
    }
    return true;
  }

  async function ensureServiceWorkerForDownload() {
    if (!prefersServiceWorkerDownload()) return false;
    try {
      await navigator.serviceWorker.register("/sw.js", { scope: "/" });
      await navigator.serviceWorker.ready;
      return !!navigator.serviceWorker.controller;
    } catch (e) {
      console.warn("conduit service worker:", e);
      return false;
    }
  }

  function startServiceWorkerFileDownload(bytes, filename, mime) {
    const ctrl = navigator.serviceWorker.controller;
    if (!ctrl) {
      throw new Error("no service worker controller");
    }
    const id =
      Math.random().toString(16).slice(2) + "-" + encodeURIComponent(filename);
    const name = fileBasename(filename) || "conduit-received.bin";
    ctrl.postMessage({
      id,
      type: "metadata",
      name,
      size: bytes.byteLength,
      filetype: mime || "application/octet-stream",
    });
    const chunkSize = 64 * 1024;
    let offset = 0;
    while (offset < bytes.byteLength) {
      const end = Math.min(offset + chunkSize, bytes.byteLength);
      const slice = bytes.subarray(offset, end);
      const copyBuffer = slice.buffer.slice(
        slice.byteOffset,
        slice.byteOffset + slice.byteLength
      );
      ctrl.postMessage({ id, type: "data", data: copyBuffer, offset }, [
        copyBuffer,
      ]);
      offset = end;
    }
    ctrl.postMessage({ id, type: "end" });
    const iframe = document.createElement("iframe");
    iframe.style.display = "none";
    iframe.src = new URL("/_/" + id, window.location.origin).href;
    document.body.appendChild(iframe);
    setTimeout(() => iframe.remove(), 120000);
  }

  function recvCliCommand(server, code) {
    if (server === DEFAULT_SERVER_URL) {
      return "conduit recv " + code;
    }
    return "conduit recv --server " + server + " " + code;
  }

  async function copyWithFlash(btn, text, restoreLabel) {
    const restore = restoreLabel != null ? restoreLabel : btn.textContent;
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = "Copied";
      setTimeout(() => {
        btn.textContent = restore;
      }, 1200);
    } catch (e) {
      btn.textContent = "Copy failed";
      setTimeout(() => {
        btn.textContent = restore;
      }, 1200);
    }
  }

  function parseHashCode() {
    const h = window.location.hash.replace(/^#/, "").trim();
    if (!h) return "";
    if (/^\d+-/.test(h)) return h;
    return "";
  }

  async function loadWasm() {
    const go = new Go();
    const res = await fetch("/main.wasm");
    if (!res.ok) {
      throw new Error("failed to load main.wasm: " + res.status);
    }
    const result = await WebAssembly.instantiateStreaming(res, go.importObject);
    go.run(result.instance);
    if (typeof globalThis.conduit === "undefined") {
      throw new Error("WASM did not register globalThis.conduit");
    }
    if (typeof globalThis.conduit.openSender !== "function") {
      throw new Error("WASM did not register conduit.openSender");
    }
    if (typeof globalThis.conduit.qr !== "function") {
      throw new Error("WASM did not register conduit.qr");
    }
  }

  // renderQR draws text as a QR code into host.
  function renderQR(host, text) {
    host.replaceChildren();
    if (!text || typeof globalThis.conduit?.qr !== "function") {
      return;
    }
    const result = globalThis.conduit.qr(text);
    if (!result) return;
    const { size, modules } = result;
    const quiet = 4;
    const cells = size + quiet * 2;
    const cellPx = 5;
    const cssSize = cells * cellPx;
    const dpr = Math.max(1, Math.floor(window.devicePixelRatio || 1));
    const canvas = document.createElement("canvas");
    canvas.width = cssSize * dpr;
    canvas.height = cssSize * dpr;
    canvas.style.width = cssSize + "px";
    canvas.style.height = cssSize + "px";
    const ctx = canvas.getContext("2d");
    ctx.fillStyle = "#ffffff";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.fillStyle = "#000000";
    const px = cellPx * dpr;
    for (let y = 0; y < size; y++) {
      for (let x = 0; x < size; x++) {
        if (modules[y * size + x]) {
          ctx.fillRect((quiet + x) * px, (quiet + y) * px, px, px);
        }
      }
    }
    host.appendChild(canvas);
  }

  // Session-mode UI state machine.
  //
  // States: "idle" → "opening" → "paired" → "closing" → "closed".
  //
  // - idle: phrase input + Open button. Empty submit creates a new session
  //   (sender role); a typed phrase joins one (receiver role). The role
  //   distinction only matters at handshake time; once paired both peers
  //   have the same controls.
  // - paired: dropzone + paste + transfer list. Either side can push
  //   files/text; inbound transfers are appended to the list as they arrive.
  // - closed: "Open another" button to return to idle.
  function wireUI(serviceWorkerDownloadOk) {
    const serverUrl = $("serverUrl");
    serverUrl.value = defaultServerUrl();
    if (!isDefaultOrigin()) {
      const field = serverUrl.closest("label.field");
      if (field) field.style.display = "";
    }

    const idlePanel = $("idlePanel");
    const createBtn = $("createBtn");
    const joinForm = $("joinForm");
    const phraseInput = $("phraseInput");
    const joinBtn = $("joinBtn");
    const idleStatus = $("idleStatus");

    function updateJoinEnabled() {
      joinBtn.disabled = phraseInput.value.trim().length === 0;
    }
    phraseInput.addEventListener("input", updateJoinEnabled);

    const pairedPanel = $("pairedPanel");
    const hostCode = $("hostCode");
    const codeText = $("codeText");
    const shareBrowserUrl = $("shareBrowserUrl");
    const shareCliCmd = $("shareCliCmd");
    const copyBrowserUrlBtn = $("copyBrowserUrlBtn");
    const copyCliCmdBtn = $("copyCliCmdBtn");
    const qrHost = $("qrHost");
    const dropzone = $("dropzone");
    const fileInput = $("fileInput");
    const pickBtn = $("pickBtn");
    const pasteBtn = $("pasteBtn");
    const transferList = $("transferList");
    const pairedStatus = $("pairedStatus");
    const closeBtn = $("closeBtn");

    const closedPanel = $("closedPanel");
    const reopenBtn = $("reopenBtn");

    let sessionId = null;
    let transferCount = 0;

    copyBrowserUrlBtn.addEventListener("click", () => {
      copyWithFlash(copyBrowserUrlBtn, shareBrowserUrl.textContent || "", "Copy");
    });
    copyCliCmdBtn.addEventListener("click", () => {
      copyWithFlash(copyCliCmdBtn, shareCliCmd.textContent || "", "Copy");
    });

    function gotoIdle() {
      idlePanel.hidden = false;
      pairedPanel.hidden = true;
      closedPanel.hidden = true;
      hostCode.hidden = true;
      transferList.hidden = true;
      transferList.replaceChildren();
      transferCount = 0;
      sessionId = null;
      setStatus(idleStatus, "", null);
      setStatus(pairedStatus, "", null);
      qrHost.replaceChildren();
      const hashCode = parseHashCode();
      phraseInput.value = hashCode;
      createBtn.disabled = false;
      phraseInput.disabled = false;
      updateJoinEnabled();
      if (hashCode) phraseInput.focus();
    }

    function gotoPaired(hostCodeData) {
      idlePanel.hidden = true;
      pairedPanel.hidden = false;
      closedPanel.hidden = true;
      if (hostCodeData) {
        codeText.textContent = hostCodeData.code;
        shareBrowserUrl.textContent = hostCodeData.link;
        shareCliCmd.textContent = hostCodeData.cli;
        renderQR(qrHost, hostCodeData.link);
        hostCode.hidden = false;
      } else {
        hostCode.hidden = true;
      }
      transferList.hidden = false;
    }

    function gotoClosed() {
      idlePanel.hidden = true;
      pairedPanel.hidden = true;
      closedPanel.hidden = false;
    }

    function appendTransfer(direction, name, totalBytes) {
      transferList.hidden = false;
      const li = document.createElement("li");
      li.className = "transfer transfer-" + direction;
      const icon = document.createElement("span");
      icon.className = "transfer-icon";
      icon.textContent = direction === "out" ? "↑" : "↓";
      const nameEl = document.createElement("span");
      nameEl.className = "transfer-name";
      nameEl.textContent = name || "untitled";
      const bytes = document.createElement("span");
      bytes.className = "transfer-bytes";
      bytes.textContent = totalBytes != null ? formatBytes(totalBytes) : "";
      const status = document.createElement("span");
      status.className = "transfer-status";
      li.appendChild(icon);
      li.appendChild(nameEl);
      li.appendChild(bytes);
      li.appendChild(status);
      transferList.appendChild(li);
      transferCount++;
      return { li, name: nameEl, bytes, status };
    }

    function buildHostCodeData(server, code) {
      const link = `${window.location.origin}${window.location.pathname}#${code}`;
      const cli = recvCliCommand(server, code);
      return { code, link, cli };
    }

    // Inbound transfers arrive as a 3-event stream from the WASM bridge:
    // start (preamble decoded) → progress (bytes accumulating) → end
    // (full payload buffered). currentInbound holds the row + progress
    // updater between events. Sessions deliver one inbound transfer at a
    // time, so a single slot is enough.
    let currentInbound = null;

    function onIncomingStart(preamble) {
      const name = preamble.name || (preamble.kind === "text" ? "text" : "file");
      const total = preamble.size != null && preamble.size >= 0 ? preamble.size : null;
      const row = appendTransfer("in", name, total);
      row.status.textContent = "receiving";
      currentInbound = {
        row,
        preamble,
        update: makeRowProgress(row, "receiving"),
      };
    }

    function onIncomingProgress(done, total) {
      if (!currentInbound) return;
      // total<0 means streaming source (preamble.size == -1); pass through
      // so makeRowProgress shows a byte counter without a percentage/ETA.
      currentInbound.update(done, total);
    }

    function onIncomingEnd(preamble, bytesUA) {
      const bytes = new Uint8Array(bytesUA);
      // If we missed onTransferStart for any reason, build a row on the fly
      // so the user still gets a record of the inbound transfer.
      if (!currentInbound) {
        onIncomingStart(preamble);
      }
      const { row } = currentInbound;
      currentInbound = null;
      row.bytes.textContent = formatBytes(bytes.byteLength);
      row.status.textContent = "received";
      row.li.classList.add("ok");
      if (isTextPayload(preamble.kind, preamble.mime)) {
        const text = decodeUtf8(bytes);
        const pre = document.createElement("pre");
        pre.className = "transfer-text-body";
        pre.textContent = text;
        row.li.appendChild(pre);
        const copy = document.createElement("button");
        copy.type = "button";
        copy.className = "copy-inline transfer-copy";
        copy.textContent = "Copy";
        copy.addEventListener("click", () => {
          copyWithFlash(copy, text, "Copy");
        });
        row.li.appendChild(copy);
      } else {
        const type = preamble.mime || "application/octet-stream";
        const outName = preamble.name || "conduit-received.bin";
        const blob = new Blob([bytes], { type });
        const useSw =
          serviceWorkerDownloadOk && navigator.serviceWorker.controller;
        if (useSw) {
          try {
            startServiceWorkerFileDownload(bytes, outName, type);
          } catch (e) {
            console.warn("conduit sw download:", e);
            queueMicrotask(() => triggerBlobDownload(blob, outName));
          }
        } else {
          queueMicrotask(() => triggerBlobDownload(blob, outName));
        }
        const dlBtn = document.createElement("button");
        dlBtn.type = "button";
        dlBtn.className = "copy-inline transfer-copy";
        dlBtn.textContent = "Re-download";
        dlBtn.addEventListener("click", () => triggerBlobDownload(blob, outName));
        row.li.appendChild(dlBtn);
      }
    }

    function pushFile(file) {
      if (sessionId == null) return;
      const row = appendTransfer("out", file.name, file.size);
      row.status.textContent = "sending";
      const onProgress = makeRowProgress(row, "sending");
      file.arrayBuffer().then((ab) => {
        const buf = new Uint8Array(ab);
        const mime = file.type || "application/octet-stream";
        globalThis.conduit.sessionPushFile(
          sessionId,
          buf,
          file.name,
          mime,
          onProgress,
          (err) => {
            if (err != null && err !== undefined) {
              row.status.textContent = String(err);
              row.li.classList.add("err");
              return;
            }
            row.bytes.textContent = formatBytes(file.size);
            row.status.textContent = "sent";
            row.li.classList.add("ok");
          }
        );
      });
    }

    // walkEntry recurses a FileSystemEntry (from drag-drop's
    // webkitGetAsEntry) and returns flat {path, file} records, where path is
    // a slash-separated relative path mirroring the dropped tree. Mirrors
    // what the CLI's tar producer emits when given a directory.
    async function walkEntry(entry, prefix) {
      const path = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.isFile) {
        const file = await new Promise((resolve, reject) =>
          entry.file(resolve, reject)
        );
        return [{ path, file }];
      }
      if (entry.isDirectory) {
        const reader = entry.createReader();
        const out = [];
        for (;;) {
          const batch = await new Promise((resolve, reject) =>
            reader.readEntries(resolve, reject)
          );
          if (!batch || batch.length === 0) break;
          for (const sub of batch) {
            const inner = await walkEntry(sub, path);
            for (const it of inner) out.push(it);
          }
        }
        return out;
      }
      return [];
    }

    // entriesFromDataTransfer flattens a DataTransfer.items list into
    // {path, file} records, walking any directory entries. Falls back to
    // dt.files (no path-preserving info) when the browser does not expose
    // webkitGetAsEntry.
    async function entriesFromDataTransfer(dt) {
      const items = dt.items ? Array.from(dt.items) : [];
      const recs = [];
      let walked = false;
      for (const item of items) {
        if (item.kind !== "file") continue;
        const entry = item.webkitGetAsEntry && item.webkitGetAsEntry();
        if (entry) {
          walked = true;
          const inner = await walkEntry(entry, "");
          for (const it of inner) recs.push(it);
        }
      }
      if (!walked) {
        const files = dt.files ? Array.from(dt.files) : [];
        for (const f of files) recs.push({ path: f.name, file: f });
      }
      return recs;
    }

    // pushItems decides between the single-file path (one file dropped, no
    // directory hierarchy) and the tar path (multiple files or any folder
    // contents). Mirrors the CLI: 1 file → kind=file; otherwise kind=tar.
    async function pushItems(items) {
      if (sessionId == null || items.length === 0) return;
      if (items.length === 1 && !items[0].path.includes("/")) {
        pushFile(items[0].file);
        return;
      }
      const total = items.reduce((acc, it) => acc + it.file.size, 0);
      const root = items[0].path.split("/")[0];
      const displayName =
        items.length > 1
          ? `${root} (+${items.length - 1})`
          : root || "files.tar";
      const row = appendTransfer("out", displayName, total);
      row.status.textContent = "sending";
      const entries = [];
      for (const it of items) {
        const buf = new Uint8Array(await it.file.arrayBuffer());
        entries.push({ name: it.path, data: buf });
      }
      const onProgress = makeRowProgress(row, "sending");
      globalThis.conduit.sessionPushTar(
        sessionId,
        entries,
        displayName,
        onProgress,
        (err) => {
          if (err != null && err !== undefined) {
            row.status.textContent = String(err);
            row.li.classList.add("err");
            return;
          }
          // The tar stream is slightly larger than the sum of file sizes
          // (PAX headers, padding); the row was created with the file-bytes
          // total to keep the display intuitive, so leave bytes alone here.
          row.status.textContent = "sent";
          row.li.classList.add("ok");
        }
      );
    }

    function pushText(text) {
      if (sessionId == null) return;
      const size = new TextEncoder().encode(text).length;
      const row = appendTransfer("out", "text", size);
      row.status.textContent = "sending";
      const onProgress = makeRowProgress(row, "sending");
      globalThis.conduit.sessionPushText(sessionId, text, onProgress, (err) => {
        if (err != null && err !== undefined) {
          row.status.textContent = String(err);
          row.li.classList.add("err");
          return;
        }
        row.bytes.textContent = formatBytes(size);
        row.status.textContent = "sent";
        row.li.classList.add("ok");
      });
    }

    pickBtn.addEventListener("click", () => fileInput.click());
    fileInput.addEventListener("change", () => {
      const files = fileInput.files ? Array.from(fileInput.files) : [];
      if (files.length === 0) return;
      const items = files.map((f) => ({ path: f.name, file: f }));
      pushItems(items).catch((err) => {
        setStatus(
          pairedStatus,
          "Send failed: " + (err && err.message ? err.message : err),
          "err"
        );
      });
      fileInput.value = "";
    });

    ["dragenter", "dragover"].forEach((ev) => {
      dropzone.addEventListener(ev, (e) => {
        e.preventDefault();
        e.stopPropagation();
        dropzone.classList.add("drag");
      });
    });
    ["dragleave", "drop"].forEach((ev) => {
      dropzone.addEventListener(ev, (e) => {
        e.preventDefault();
        e.stopPropagation();
        dropzone.classList.remove("drag");
      });
    });
    dropzone.addEventListener("drop", async (e) => {
      if (sessionId == null) return;
      try {
        const items = await entriesFromDataTransfer(e.dataTransfer);
        if (items.length > 0) await pushItems(items);
      } catch (err) {
        setStatus(
          pairedStatus,
          "Drop failed: " + (err && err.message ? err.message : err),
          "err"
        );
      }
    });
    dropzone.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        fileInput.click();
      }
    });
    dropzone.addEventListener("click", (e) => {
      if (e.target === dropzone || e.target.closest("p")) {
        if (e.target !== pickBtn && !e.target.closest("button")) {
          fileInput.click();
        }
      }
    });

    pasteBtn.addEventListener("click", async () => {
      if (pasteBtn.disabled) return;
      try {
        const text = await navigator.clipboard.readText();
        if (!text || !text.trim()) {
          setStatus(pairedStatus, "Clipboard is empty.", null);
          return;
        }
        pushText(text);
      } catch (err) {
        setStatus(pairedStatus, "Could not read clipboard: " + (err && err.message ? err.message : err), "err");
      }
    });

    // Desktop Ctrl+V/Cmd+V: paste anywhere on the paired panel sends text.
    document.addEventListener("paste", (e) => {
      if (pairedPanel.hidden || pasteBtn.disabled) return;
      const target = e.target;
      if (target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement) return;
      const text = e.clipboardData && e.clipboardData.getData("text/plain");
      if (!text || !text.trim()) return;
      e.preventDefault();
      pushText(text);
    });

    closeBtn.addEventListener("click", () => {
      if (sessionId == null) return;
      setStatus(pairedStatus, "Closing…", null);
      closeBtn.disabled = true;
      globalThis.conduit.sessionClose(sessionId);
    });

    reopenBtn.addEventListener("click", () => {
      gotoIdle();
      openSession("");
    });

    function openSession(phrase) {
      const server = serverUrl.value.trim() || defaultServerUrl();
      createBtn.disabled = true;
      joinBtn.disabled = true;
      phraseInput.disabled = true;
      setStatus(idleStatus, "Connecting…", null);

      const callbacks = {
        onCode: (code) => {
          if (!phrase) {
            const data = buildHostCodeData(server, code);
            // Display the code immediately while waiting for the peer.
            codeText.textContent = data.code;
            shareBrowserUrl.textContent = data.link;
            shareCliCmd.textContent = data.cli;
            renderQR(qrHost, data.link);
            hostCode.hidden = false;
            // Move into the paired panel pre-pair so the user can see the code
            // and QR; transfer controls get enabled in onPaired.
            idlePanel.hidden = true;
            pairedPanel.hidden = false;
            transferList.hidden = false;
            setStatus(pairedStatus, "Waiting for peer…", null);
            // Disable inputs until paired.
            dropzone.classList.add("disabled");
            pasteBtn.disabled = true;
          }
        },
        onPaired: (route) => {
          if (!phrase) {
            setConnectedStatus(pairedStatus, route);
          } else {
            gotoPaired(null);
            setConnectedStatus(pairedStatus, route);
          }
          dropzone.classList.remove("disabled");
          pasteBtn.disabled = false;
          closeBtn.disabled = false;
        },
        onTransferStart: onIncomingStart,
        onTransferProgress: onIncomingProgress,
        onTransferEnd: onIncomingEnd,
        onError: (msg) => {
          setStatus(pairedStatus, String(msg), "err");
          setStatus(idleStatus, String(msg), "err");
          createBtn.disabled = false;
          phraseInput.disabled = false;
          updateJoinEnabled();
          sessionId = null;
        },
        onClosed: () => {
          gotoClosed();
        },
      };

      if (phrase) {
        sessionId = globalThis.conduit.openReceiver(server, phrase, callbacks);
      } else {
        sessionId = globalThis.conduit.openSender(server, callbacks);
      }
      if (!sessionId) {
        setStatus(idleStatus, "Could not open session.", "err");
        createBtn.disabled = false;
        phraseInput.disabled = false;
        updateJoinEnabled();
      }
    }

    createBtn.addEventListener("click", () => openSession(""));
    joinForm.addEventListener("submit", (e) => {
      e.preventDefault();
      openSession(phraseInput.value.trim());
    });

    window.addEventListener("hashchange", () => {
      if (idlePanel.hidden) return;
      const c = parseHashCode();
      if (c) {
        phraseInput.value = c;
        updateJoinEnabled();
      }
    });

    gotoIdle();

    // If the URL arrived with a code in the hash, auto-submit so the receiver
    // flow runs without an extra click.
    const initialHash = parseHashCode();
    if (initialHash) {
      phraseInput.value = initialHash;
      updateJoinEnabled();
      joinForm.requestSubmit();
    }
  }

  loadWasm()
    .then(() => ensureServiceWorkerForDownload())
    .then((swOk) => wireUI(swOk))
    .catch((e) => {
      const p = document.createElement("p");
      p.className = "status err";
      p.textContent = "WASM: " + (e.message || String(e));
      document.body.prepend(p);
    });
})();
