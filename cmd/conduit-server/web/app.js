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

  function wireUI() {
    const serverUrl = $("serverUrl");
    serverUrl.value = defaultServerUrl();

    const dropzone = $("dropzone");
    const fileInput = $("fileInput");
    const pickBtn = $("pickBtn");
    const sendFileName = $("sendFileName");
    const sendCode = $("sendCode");
    const codeText = $("codeText");
    const qrHost = $("qrHost");
    const sendProgress = $("sendProgress");
    const sendBar = $("sendBar");
    const sendStatus = $("sendStatus");

    const recvCode = $("recvCode");
    const recvBtn = $("recvBtn");
    const recvProgress = $("recvProgress");
    const recvBar = $("recvBar");
    const recvStatus = $("recvStatus");
    const recvDownload = $("recvDownload");
    const recvLink = $("recvLink");

    const hashCode = parseHashCode();
    if (hashCode) {
      recvCode.value = hashCode;
    }

    pickBtn.addEventListener("click", () => fileInput.click());

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

    async function runSend() {
      const f = fileInput.files && fileInput.files[0];
      if (!f) {
        setStatus(sendStatus, "Choose a file first.", "err");
        return;
      }
      const server = serverUrl.value.trim() || defaultServerUrl();
      const buf = new Uint8Array(await f.arrayBuffer());

      sendCode.hidden = true;
      sendProgress.hidden = false;
      recvDownload.hidden = true;
      setBar(sendBar, 0);
      setStatus(sendStatus, "Connecting…", null);

      globalThis.conduit.send(
        server,
        buf,
        f.name,
        function onCode(code) {
          codeText.textContent = code;
          sendCode.hidden = false;
          const link = `${window.location.origin}${window.location.pathname}#${code}`;
          renderQR(qrHost, link);
        },
        function onProgress(done, total) {
          if (total > 0) {
            setBar(sendBar, done / total);
          }
        },
        function onDone(err) {
          sendProgress.hidden = true;
          if (err != null && err !== undefined) {
            setStatus(sendStatus, String(err), "err");
            return;
          }
          setStatus(sendStatus, "Sent successfully.", "ok");
        }
      );
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
        setStatus(recvStatus, "Enter the code from the sender.", "err");
        return;
      }
      const server = serverUrl.value.trim() || defaultServerUrl();
      recvProgress.hidden = false;
      recvProgress.classList.add("indet");
      recvDownload.hidden = true;
      setBar(recvBar, 0);
      setStatus(recvStatus, "Connecting…", null);

      globalThis.conduit.recv(
        server,
        code,
        function onProgress(n) {
          recvProgress.classList.remove("indet");
          setBar(recvBar, 1);
          setStatus(recvStatus, "Receiving… " + n + " bytes", null);
        },
        function onDone(err, data, filename) {
          recvProgress.hidden = true;
          recvProgress.classList.remove("indet");
          if (err != null && err !== undefined) {
            setStatus(recvStatus, String(err), "err");
            return;
          }
          const bytes = new Uint8Array(data);
          const blob = new Blob([bytes], { type: "application/octet-stream" });
          const url = URL.createObjectURL(blob);
          recvLink.href = url;
          recvLink.download = filename || "conduit-received.bin";
          recvDownload.hidden = false;
          setStatus(recvStatus, "Received " + bytes.length + " bytes.", "ok");
        }
      );
    });

    if (hashCode) {
      recvBtn.click();
    }
  }

  loadWasm()
    .then(() => {
      wireUI();
    })
    .catch((e) => {
      const p = document.createElement("p");
      p.className = "status err";
      p.textContent = "Failed to load WebAssembly: " + (e.message || String(e));
      document.body.prepend(p);
    });
})();
