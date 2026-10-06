"use strict";

// Frames carry the operator's shell. Those bytes can include that machine's
// BAT token when the operator prints the file. Showing them is the terminal;
// copying a frame into a log, an audit row, persistence, or an error string
// is not. This file therefore never logs a frame, and it copies `reason`
// onto the page only as text. No frame carries a session identifier. The
// socket path is the binding, and the Hub rejects a frame that tries to name one.
(function () {
  var status = document.getElementById("terminal-status");
  var container = document.getElementById("terminal");
  var openedMessage = "這個終端已開啟，可以輸入。";
  var closedMessage = "這個終端的連線已結束，未再接受輸入。請回到機器頁重新開啟。";
  var unavailableMessage = "這個終端沒有開啟，未再接受輸入。請回到機器頁重新開啟。";
  var malformedMessage = "這個終端收到無法顯示的內容，已停止接受輸入。請回到機器頁重新開啟。";
  // 32 KiB is the raw byte budget before base64. A paste is how an operator
  // produces one oversized frame, so the bytes are split and sent in order.
  var maxInputBytes = 32 * 1024;
  var interactive = false;
  var settled = false;
  var socket = null;
  var latestCols = 0;
  var latestRows = 0;

  function show(text) {
    status.textContent = text;
  }

  function settle(text) {
    interactive = false;
    settled = true;
    show(text);
  }

  if (!status || !container) {
    return;
  }

  window.addEventListener("beforeunload", function (event) {
    if (!interactive) {
      return;
    }
    event.preventDefault();
    event.returnValue = "";
  });

  // xterm's UMD build copies Terminal onto globalThis. The fit bundle assigns
  // its module, so the constructor is that module's FitAddon export.
  var TerminalCtor = globalThis.Terminal;
  var FitCtor = globalThis.FitAddon && globalThis.FitAddon.FitAddon;
  if (typeof TerminalCtor !== "function" || typeof FitCtor !== "function") {
    show(unavailableMessage);
    return;
  }

  var term = new TerminalCtor({cursorBlink: true, fontSize: 14});
  var fit = new FitCtor();
  term.loadAddon(fit);
  term.open(container);
  fit.fit();

  function clampGeometry(value) {
    if (!Number.isInteger(value) || value < 1) {
      return 1;
    }
    if (value > 10000) {
      return 10000;
    }
    return value;
  }

  function sendResize() {
    if (!socket || socket.readyState !== WebSocket.OPEN || latestCols < 1 || latestRows < 1) {
      return;
    }
    socket.send(JSON.stringify({
      type: "resize",
      cols: clampGeometry(latestCols),
      rows: clampGeometry(latestRows)
    }));
  }

  function rememberSize(cols, rows) {
    if (!Number.isInteger(cols) || !Number.isInteger(rows)) {
      return;
    }
    latestCols = cols;
    latestRows = rows;
    sendResize();
  }

  rememberSize(term.cols, term.rows);
  term.onResize(function (size) {
    rememberSize(size.cols, size.rows);
  });
  window.addEventListener("resize", function () {
    fit.fit();
  });

  function encodeBase64(bytes) {
    var binary = "";
    for (var i = 0; i < bytes.length; i++) {
      binary += String.fromCharCode(bytes[i]);
    }
    return btoa(binary);
  }

  function decodeBase64(value) {
    var binary = atob(value);
    var bytes = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return bytes;
  }

  var encoder = new TextEncoder();
  term.onData(function (data) {
    if (!interactive || !socket || socket.readyState !== WebSocket.OPEN) {
      return;
    }
    var bytes = encoder.encode(data);
    var offset = 0;
    while (offset < bytes.length) {
      var end = offset + maxInputBytes;
      if (end > bytes.length) {
        end = bytes.length;
      }
      // xterm hands onData a valid UTF-8 string. Only this split can break a
      // code point, and the agent rejects a frame that is not valid on its own.
      if (end < bytes.length) {
        while (end > offset && (bytes[end] & 0xC0) === 0x80) {
          end--;
        }
      }
      socket.send(JSON.stringify({
        type: "input",
        data: encodeBase64(bytes.subarray(offset, end))
      }));
      offset = end;
    }
  });

  var socketURL = (location.protocol === "https:" ? "wss://" : "ws://") + location.host + location.pathname + "/socket";
  socket = new WebSocket(socketURL, "aiintune.operator-terminal.v1");
  socket.onopen = function () {
    sendResize();
  };
  // onerror carries no operator-facing state and is followed by onclose.
  // Leaving it empty keeps that transport error out of developer tools.
  socket.onerror = function () {};
  socket.onclose = function () {
    interactive = false;
    if (!settled) {
      settle(closedMessage);
    }
  };
  socket.onmessage = function (event) {
    if (typeof event.data !== "string") {
      settle(malformedMessage);
      return;
    }
    var message;
    try {
      message = JSON.parse(event.data);
    } catch (err) {
      settle(malformedMessage);
      return;
    }
    if (!message || typeof message.type !== "string") {
      settle(malformedMessage);
      return;
    }
    if (message.type === "ready") {
      if (!settled) {
        interactive = true;
        term.focus();
        show(openedMessage);
      }
      return;
    }
    if (message.type === "output") {
      if (typeof message.data !== "string") {
        settle(malformedMessage);
        return;
      }
      try {
        term.write(decodeBase64(message.data));
      } catch (err) {
        settle(malformedMessage);
      }
      return;
    }
    if (message.type === "exit") {
      if (typeof message.code !== "number" || !Number.isInteger(message.code)) {
        settle(malformedMessage);
        return;
      }
      settle("這個終端已結束，結束代碼 " + String(message.code) + "，未再接受輸入。請回到機器頁重新開啟。");
      return;
    }
    if (message.type === "error") {
      if (typeof message.reason !== "string") {
        settle(malformedMessage);
        return;
      }
      // `reason` is Hub copy. Display it unchanged and do not classify it.
      settle(message.reason);
      return;
    }
    settle(malformedMessage);
  };
})();
