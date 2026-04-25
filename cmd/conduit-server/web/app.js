/* global Go, QRCode */

(function () {
  "use strict";

  const $ = (id) => document.getElementById(id);

  function defaultServerUrl() {
    return window.location.origin;
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

  function setBar(barEl, frac) {
    const p = Math.max(0, Math.min(1, frac));
    barEl.style.width = (p * 100).toFixed(2) + "%";
  }

  function isTextPayload(kind, mime) {
    if (kind === "text") {
      return true;
    }
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
    setTimeout(() => {
      URL.revokeObjectURL(url);
    }, 5000);
  }

  function fileBasename(name) {
    if (!name || typeof name !== "string") {
      return "";
    }
    const parts = name.replace(/\\/g, "/").split("/");
    const base = parts.pop();
    return base || "";
  }

  /** Same idea as webwormhole: Safari cannot rely on SW-driven downloads. */
  function prefersServiceWorkerDownload() {
    if (!("serviceWorker" in navigator)) {
      return false;
    }
    if (!window.isSecureContext) {
      return false;
    }
    if (
      /Safari/.test(navigator.userAgent) &&
      !/Chrome|Chromium/.test(navigator.userAgent)
    ) {
      return false;
    }
    return true;
  }

  async function ensureServiceWorkerForDownload() {
    if (!prefersServiceWorkerDownload()) {
      return false;
    }
    try {
      await navigator.serviceWorker.register("/sw.js", { scope: "/" });
      await navigator.serviceWorker.ready;
      return !!navigator.serviceWorker.controller;
    } catch (e) {
      console.warn("conduit service worker:", e);
      return false;
    }
  }

  /**
   * Stream bytes to the service worker, then open a hidden iframe on /_/id so
   * the worker responds with Content-Disposition: attachment (Web Wormhole pattern).
   */
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
      ctrl.postMessage(
        { id, type: "data", data: copyBuffer, offset },
        [copyBuffer]
      );
      offset = end;
    }
    ctrl.postMessage({ id, type: "end" });

    const iframe = document.createElement("iframe");
    iframe.style.display = "none";
    iframe.src = new URL("/_/" + id, window.location.origin).href;
    document.body.appendChild(iframe);
    setTimeout(() => {
      iframe.remove();
    }, 120000);
  }

  function recvCliCommand(server, code) {
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
    if (!h) {
      return "";
    }
    if (/^\d+-/.test(h)) {
      return h;
    }
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
    if (typeof globalThis.conduit.sendText !== "function") {
      throw new Error("WASM did not register conduit.sendText");
    }
  }

  function renderQR(host, text) {
    host.replaceChildren();
    if (!text || typeof QRCode === "undefined") {
      return;
    }
    QRCode.toCanvas(
      text,
      { width: 160, margin: 2, color: { dark: "#000000ff", light: "#ffffffff" } },
      (err, canvas) => {
        if (err) {
          console.warn("qrcode:", err);
          return;
        }
        host.appendChild(canvas);
      }
    );
  }

  function wireUI(serviceWorkerDownloadOk) {
    const serverUrl = $("serverUrl");
    serverUrl.value = defaultServerUrl();

    const dropzone = $("dropzone");
    const fileInput = $("fileInput");
    const pasteTarget = $("pasteTarget");
    const pickBtn = $("pickBtn");
    const sendFileName = $("sendFileName");
    const sendCode = $("sendCode");
    const codeText = $("codeText");
    const shareBrowserUrl = $("shareBrowserUrl");
    const shareCliCmd = $("shareCliCmd");
    const copyBrowserUrlBtn = $("copyBrowserUrlBtn");
    const copyCliCmdBtn = $("copyCliCmdBtn");
    const qrHost = $("qrHost");
    const sendProgress = $("sendProgress");
    const sendBar = $("sendBar");
    const sendStatus = $("sendStatus");

    const recvCode = $("recvCode");
    const recvBtn = $("recvBtn");
    const recvProgress = $("recvProgress");
    const recvBar = $("recvBar");
    const recvStatus = $("recvStatus");
    const recvText = $("recvText");
    const recvTextLabel = $("recvTextLabel");
    const recvTextBody = $("recvTextBody");
    const recvCopyBtn = $("recvCopyBtn");

    recvCopyBtn.addEventListener("click", async () => {
      const text = recvTextBody.textContent || "";
      await copyWithFlash(recvCopyBtn, text, "Copy");
    });

    copyBrowserUrlBtn.addEventListener("click", () => {
      copyWithFlash(copyBrowserUrlBtn, shareBrowserUrl.textContent || "", "Copy");
    });

    copyCliCmdBtn.addEventListener("click", () => {
      copyWithFlash(copyCliCmdBtn, shareCliCmd.textContent || "", "Copy");
    });

    const hashCode = parseHashCode();
    if (hashCode) {
      recvCode.value = hashCode;
    }

    pickBtn.addEventListener("click", () => fileInput.click());

    pasteTarget.addEventListener("paste", (e) => {
      const text = e.clipboardData && e.clipboardData.getData("text/plain");
      if (text === "") {
        return;
      }
      if (!text.trim()) {
        return;
      }
      e.preventDefault();
      if (!sendProgress.hidden) {
        setStatus(sendStatus, "Busy.", "err");
        return;
      }
      pasteTarget.value = "";
      runSendText(text);
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
    dropzone.addEventListener("drop", (e) => {
      const f = e.dataTransfer.files && e.dataTransfer.files[0];
      if (f) {
        fileInput.files = e.dataTransfer.files;
        sendFileName.textContent = f.name;
        runSend().catch((err) => {
          setStatus(sendStatus, err.message || String(err), "err");
          sendProgress.hidden = true;
        });
      }
    });

    dropzone.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        fileInput.click();
      }
    });

    window.addEventListener("hashchange", () => {
      const c = parseHashCode();
      if (c) {
        recvCode.value = c;
      }
    });

    dropzone.addEventListener("click", (e) => {
      if (e.target === dropzone || e.target.closest("p")) {
        if (e.target !== pickBtn && !e.target.closest("button")) {
          fileInput.click();
        }
      }
    });

    function sendCallbacks(server) {
      return {
        onCode(code) {
          codeText.textContent = code;
          sendCode.hidden = false;
          const link = `${window.location.origin}${window.location.pathname}#${code}`;
          shareBrowserUrl.textContent = link;
          shareCliCmd.textContent = recvCliCommand(server, code);
          renderQR(qrHost, link);
        },
        onProgress(done, total) {
          if (total > 0) {
            setBar(sendBar, done / total);
          }
        },
        onDone(err) {
          sendProgress.hidden = true;
          if (err != null && err !== undefined) {
            setStatus(sendStatus, String(err), "err");
            return;
          }
          setStatus(sendStatus, "Sent.", "ok");
        },
      };
    }

    async function runSend() {
      if (!sendProgress.hidden) {
        setStatus(sendStatus, "Busy.", "err");
        return;
      }
      const f = fileInput.files && fileInput.files[0];
      if (!f) {
        setStatus(sendStatus, "Pick a file.", "err");
        return;
      }
      const server = serverUrl.value.trim() || defaultServerUrl();
      const buf = new Uint8Array(await f.arrayBuffer());

      sendCode.hidden = true;
      sendProgress.hidden = false;
      setBar(sendBar, 0);
      setStatus(sendStatus, "Connecting", null);

      const cb = sendCallbacks(server);
      globalThis.conduit.send(server, buf, f.name, cb.onCode, cb.onProgress, cb.onDone);
    }

    function runSendText(text) {
      if (!sendProgress.hidden) {
        setStatus(sendStatus, "Busy.", "err");
        return;
      }
      const server = serverUrl.value.trim() || defaultServerUrl();
      sendCode.hidden = true;
      sendProgress.hidden = false;
      setBar(sendBar, 0);
      sendFileName.textContent = "Text · " + text.length;
      setStatus(sendStatus, "Connecting", null);

      const cb = sendCallbacks(server);
      globalThis.conduit.sendText(server, text, cb.onCode, cb.onProgress, cb.onDone);
    }

    fileInput.addEventListener("change", () => {
      const f = fileInput.files && fileInput.files[0];
      sendFileName.textContent = f ? f.name : "";
      if (!f) {
        return;
      }
      runSend().catch((e) => {
        setStatus(sendStatus, e.message || String(e), "err");
        sendProgress.hidden = true;
      });
    });

    recvBtn.addEventListener("click", () => {
      const code = recvCode.value.trim();
      if (!code) {
        setStatus(recvStatus, "Code?", "err");
        return;
      }
      const server = serverUrl.value.trim() || defaultServerUrl();
      recvProgress.hidden = false;
      recvProgress.classList.add("indet");
      recvText.hidden = true;
      recvTextBody.textContent = "";
      setBar(recvBar, 0);
      setStatus(recvStatus, "Connecting", null);

      globalThis.conduit.recv(
        server,
        code,
        function onProgress(n) {
          recvProgress.classList.remove("indet");
          setBar(recvBar, 1);
          setStatus(recvStatus, "↓ " + n + " B", null);
        },
        function onDone(err, data, filename, kind, mime) {
          recvProgress.hidden = true;
          recvProgress.classList.remove("indet");
          if (err != null && err !== undefined) {
            setStatus(recvStatus, String(err), "err");
            return;
          }
          const bytes = new Uint8Array(data);
          if (isTextPayload(kind, mime)) {
            const text = decodeUtf8(bytes);
            recvTextBody.textContent = text;
            recvTextLabel.textContent = filename || "Text";
            recvText.hidden = false;
            setStatus(recvStatus, "Done · " + bytes.length, "ok");
            return;
          }
          const type = mime || "application/octet-stream";
          const blob = new Blob([bytes], { type });
          const outName = filename || "conduit-received.bin";
          setStatus(recvStatus, "Done · " + bytes.length, "ok");

          const useSw =
            serviceWorkerDownloadOk && navigator.serviceWorker.controller;
          if (useSw) {
            try {
              startServiceWorkerFileDownload(bytes, outName, type);
            } catch (e) {
              console.warn("conduit sw download:", e);
              queueMicrotask(() => {
                triggerBlobDownload(blob, outName);
              });
            }
          } else {
            queueMicrotask(() => {
              triggerBlobDownload(blob, outName);
            });
          }
        }
      );
    });

    if (hashCode) {
      recvBtn.click();
    }
  }

  loadWasm()
    .then(() => ensureServiceWorkerForDownload())
    .then((swOk) => {
      wireUI(swOk);
    })
    .catch((e) => {
      const p = document.createElement("p");
      p.className = "status err";
      p.textContent = "WASM: " + (e.message || String(e));
      document.body.prepend(p);
    });
})();
