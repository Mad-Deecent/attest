// The enrollment page's three bits of behavior: the guided voice recording,
// the live "Save N choices" label, and deleting a voice. Everything else is
// plain server-rendered HTML.
(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };

  // --- choices --------------------------------------------------------------

  (function choices() {
    var form = $("consent"), save = $("save");
    if (!form || !save) return;
    function update() {
      var n = form.querySelectorAll('input[type="checkbox"]:checked').length;
      save.textContent = n === 0 ? save.dataset.labelNone
        : n === 1 ? save.dataset.labelOne
        : save.dataset.labelMany.replace("{n}", String(n));
    }
    form.addEventListener("change", update);
    update();
  })();

  // --- deleting a voice -------------------------------------------------------

  // The page asks attest for a ticket good only for deletion and sends it to
  // the voice service, which checks it with attest and deletes what it holds.
  (function forget() {
    var box = $("forget");
    if (!box) return;
    var ask = $("forget-ask"), confirm = $("forget-confirm"), yes = $("forget-yes"), no = $("forget-no");
    var say = $("forget-status");
    ask.addEventListener("click", function () {
      ask.hidden = true;
      confirm.hidden = false;
      say.textContent = "";
      no.focus();
    });
    no.addEventListener("click", function () {
      confirm.hidden = true;
      ask.hidden = false;
      ask.focus();
    });
    function failed(message) {
      say.textContent = message;
      yes.disabled = false;
      no.disabled = false;
      yes.textContent = "Try again";
    }
    yes.addEventListener("click", function () {
      yes.disabled = true;
      no.disabled = true;
      say.textContent = "Deleting your voice.";
      fetch("/enroll/voice-deletion-ticket", {
        method: "POST",
        credentials: "same-origin",
        headers: { "X-CSRF-Token": box.dataset.csrf }
      }).then(function (r) {
        if (r.status === 401) throw new Error("signed out");
        if (!r.ok) throw new Error("ticket");
        return r.json();
      }).then(function (t) {
        return fetch(box.dataset.voiceUrl, {
          method: "DELETE",
          headers: { "Authorization": "Bearer " + t.ticket }
        }).catch(function () { throw new Error("unreachable"); });
      }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (body) {
          if (r.ok) {
            window.location.assign("/enroll?step=manage&done=voicedeleted");
            return;
          }
          if (r.status === 500 && body.deleted === false) {
            failed("attest no longer lists your voice, but the voice service couldn't delete it. Try again; if it keeps failing, tell whoever runs this system.");
            return;
          }
          failed(typeof body.error === "string" && r.status === 502 ? body.error
            : "Your voice couldn't be deleted. Nothing changed. Try again in a moment.");
        });
      }).catch(function (err) {
        if (err.message === "signed out") {
          failed("Your sign-in has expired. Reload the page and sign in again.");
          yes.disabled = true;
          return;
        }
        failed(err.message === "unreachable"
          ? "The voice service couldn't be reached. Try again in a moment."
          : "Your voice couldn't be deleted. Try again in a moment.");
      });
    });
  })();

  // --- the guided recording -----------------------------------------------------

  // The person says short lines one at a time while a rough, in-browser
  // estimate of how long they have been speaking fills a meter. Each stretch
  // of recording is a "take"; on Finish every take so far goes straight to the
  // voice service with a fresh single-use ticket from attest. The voice
  // service decides what counts as speech and answers with how much it
  // heard. If that is not enough, the person keeps going and the next save
  // sends the earlier takes again with the new one; nothing is kept between
  // saves anywhere but in this page. attest never receives audio.
  var box = $("recorder");
  if (!box) return;

  var intro = $("voice-intro"), saved = $("voice-saved");
  var startBtn = $("start");
  var say = $("voice-status");
  var promptEl = $("prompt"), kindEl = $("prompt-kind"), countEl = $("prompt-count");
  var meter = $("meter"), fill = $("meter-fill"), tick = $("meter-tick");
  var heardEl = $("heard"), left = $("meter-left"), right = $("meter-right");
  var hint = $("rec-hint"), stateWord = $("rec-state-word");
  var canvas = $("wave");
  var buttons = { next: $("next"), finish: $("finish"), more: $("more"), retry: $("retry"), over: $("over") };
  var restartBtn = $("restart");
  var prompts = Array.prototype.map.call($("prompt-list").children, function (li) {
    return { text: li.textContent, read: li.hasAttribute("data-read") };
  });
  var TARGET = Number(box.dataset.target) || 30;
  var MIN = Number(box.dataset.min) || 15;
  var MAX_RECORDING = 100; // seconds across all takes; the voice service takes up to 120
  var STEP = 0.05;         // seconds per level reading
  var BARS = 48;
  var reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  if (!window.MediaRecorder || !navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    say.textContent = "This browser can't record audio here. You can skip this step, or come back in another browser.";
    return;
  }
  startBtn.disabled = false;

  // Ask the voice service for its own minimum; until it answers, or if it
  // can't be asked, the page uses the default it was given.
  fetch(box.dataset.voiceUrl, { method: "GET" }).then(function (r) {
    return r.ok ? r.json() : null;
  }).then(function (req) {
    if (req && Number(req.minVoicedSeconds) > 0) setMin(Number(req.minVoicedSeconds));
  }).catch(function () {});

  // Chrome and Firefox record webm/opus; Safari records mp4 (AAC).
  var mimeType = ["audio/webm;codecs=opus", "audio/mp4", "audio/ogg;codecs=opus", "audio/webm"].filter(function (t) {
    return MediaRecorder.isTypeSupported && MediaRecorder.isTypeSupported(t);
  })[0] || "";

  var takes = [];        // Blobs, oldest first
  var confirmed = 0;     // seconds of speech the voice service counted in `takes`
  var estimated = 0;     // seconds of speech this page estimated in `takes`
  var recordedBefore = 0;// seconds of recording in `takes`
  var line = 0;          // index into prompts
  var rec = null;        // the current take, while recording

  function show(names) {
    Object.keys(buttons).forEach(function (k) { buttons[k].hidden = names.indexOf(k) < 0; });
  }

  function seconds(n) { n = Math.floor(n); return n + (n === 1 ? " second" : " seconds"); }

  function setMin(m) {
    MIN = m;
    tick.style.left = Math.min(100, (MIN / TARGET) * 100) + "%";
  }
  setMin(MIN);

  function view(name) {
    box.dataset.view = name;
    $("rec-live").hidden = name !== "live";
    $("rec-saving").hidden = name !== "saving";
    $("rec-refused").hidden = name !== "refused";
    stateWord.textContent = name === "refused" ? "Paused" : "Recording";
  }

  function open() {
    box.hidden = false;
    document.body.classList.add("recording");
  }

  function close() {
    box.hidden = true;
    document.body.classList.remove("recording");
  }

  // --- the meter ------------------------------------------------------------

  function setFill(total) {
    fill.style.transform = "scaleX(" + Math.min(total / TARGET, 1) + ")";
    meter.setAttribute("aria-valuenow", String(Math.min(Math.floor(total), TARGET)));
    meter.setAttribute("aria-valuetext", seconds(total) + " of speech, of about " + TARGET);
  }

  // While recording: the estimate so far, and which button leads.
  function setHeard(total) {
    setFill(total);
    left.innerHTML = "";
    heardEl.textContent = String(Math.floor(total));
    left.appendChild(heardEl);
    left.appendChild(document.createTextNode(" s of speech"));
    right.textContent = "about " + TARGET + " s";
    var enough = total >= MIN, plenty = total >= TARGET;
    buttons.finish.disabled = !enough;
    // At the target, Finish leads and Next steps back.
    buttons.finish.classList.toggle("primary", plenty);
    buttons.next.classList.toggle("primary", !plenty);
    hint.textContent = !enough ? "Finish opens at " + Math.ceil(MIN) + " s. " + TARGET + " s recognizes you better."
      : plenty ? "That’s enough. Finish when you’re ready."
      : "You can finish now, or keep going for a better match.";
  }

  function showLine() {
    var p = prompts[line % prompts.length];
    countEl.textContent = "Line " + (line % prompts.length + 1) + " of " + prompts.length;
    kindEl.textContent = p.read ? "Read this" : "Say it like you’d ask";
    promptEl.textContent = p.text;
  }

  // --- recording a take -----------------------------------------------------

  function start() {
    show([]);
    say.textContent = "Asking to use the microphone.";
    navigator.mediaDevices.getUserMedia({
      // The voice service hears rooms, not cleaned-up calls: leave the voice
      // as it is, but let the browser even out the level.
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: true }
    }).then(begin, function () {
      var msg = "The microphone isn't available. Check that this page may use it, then try again.";
      if (takes.length) {
        refused("Paused", "The microphone isn't available.", "Check that this page may use it, then keep going. Your takes so far are still here.", ["more"]);
      } else {
        close();
        say.textContent = msg;
      }
    });
  }

  function begin(stream) {
    var r = { stream: stream, chunks: [], speech: 0, recorded: 0, lineSpeech: 0, quiet: 0, floor: 0, levels: [] };
    try {
      r.media = new MediaRecorder(stream, mimeType ? { mimeType: mimeType } : undefined);
    } catch (e) {
      r.media = new MediaRecorder(stream);
    }
    r.media.ondataavailable = function (e) { if (e.data && e.data.size) r.chunks.push(e.data); };

    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (Ctx) {
      r.ctx = new Ctx();
      if (r.ctx.resume) r.ctx.resume();
      r.analyser = r.ctx.createAnalyser();
      r.analyser.fftSize = 2048;
      r.ctx.createMediaStreamSource(stream).connect(r.analyser);
      r.buf = new Float32Array(r.analyser.fftSize);
    }
    rec = r;
    r.media.start(1000);
    r.timer = setInterval(listen, STEP * 1000);
    if (!reduceMotion) r.frame = requestAnimationFrame(draw);

    say.textContent = "";
    open();
    view("live");
    showLine();
    setHeard(confirmed);
    show(["next", "finish"]);
    restartBtn.hidden = false;
    buttons.next.focus();
  }

  // Roughly how long the person has been speaking: a reading counts as speech
  // when it is well above the room's quiet level. The voice service measures
  // properly; this only drives the meter and moves to the next line.
  function listen() {
    var r = rec;
    if (!r) return;
    r.recorded += STEP;
    var level = 0;
    if (r.analyser) {
      r.analyser.getFloatTimeDomainData(r.buf);
      var sum = 0;
      for (var i = 0; i < r.buf.length; i++) sum += r.buf[i] * r.buf[i];
      level = Math.sqrt(sum / r.buf.length);
    }
    if (!r.floor || level < r.floor) r.floor = Math.max(level, 1e-4);
    else r.floor += (level - r.floor) * 0.002;
    var speaking = level > Math.max(r.floor * 4, 0.006);
    r.levels.push({ level: level, speaking: speaking });
    if (r.levels.length > 400) r.levels.shift();
    if (reduceMotion) box.classList.toggle("hearing", speaking);

    if (speaking) {
      r.speech += STEP;
      r.lineSpeech += STEP;
      r.quiet = 0;
    } else {
      r.quiet += STEP;
    }
    // A line is done after some speech and then a pause.
    if (r.lineSpeech >= 1.2 && r.quiet >= 0.9) nextLine();
    setHeard(confirmed + r.speech);
    if (recordedBefore + r.recorded >= MAX_RECORDING) finish();
  }

  function nextLine() {
    line++;
    if (rec) { rec.lineSpeech = 0; rec.quiet = 0; }
    showLine();
  }

  // The waveform: the last few seconds as bars, in the accent while speaking.
  function draw() {
    var r = rec;
    if (!r) return;
    var dpr = window.devicePixelRatio || 1;
    var w = canvas.clientWidth, h = canvas.clientHeight;
    if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
      canvas.width = Math.round(w * dpr);
      canvas.height = Math.round(h * dpr);
    }
    var g = canvas.getContext("2d");
    var css = getComputedStyle(canvas);
    var live = css.getPropertyValue("--live").trim(), faint = css.getPropertyValue("--faint").trim();
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, w, h);
    var gap = 3, bw = (w - gap * (BARS - 1)) / BARS;
    var per = 2, data = r.levels.slice(-BARS * per);
    for (var i = 0; i < BARS; i++) {
      var k = data.length - (BARS - i) * per;
      var level = 0, speaking = false;
      for (var j = Math.max(0, k); j < k + per && j >= 0 && j < data.length; j++) {
        level = Math.max(level, data[j].level);
        speaking = speaking || data[j].speaking;
      }
      var amp = Math.min(1, Math.sqrt(level / 0.25));
      var bar = Math.max(4, amp * h);
      g.globalAlpha = speaking ? 0.45 + amp * 0.55 : 1;
      g.fillStyle = speaking ? live : faint;
      var x = i * (bw + gap), y = (h - bar) / 2;
      if (g.roundRect) { g.beginPath(); g.roundRect(x, y, bw, bar, Math.min(2, bw / 2)); g.fill(); }
      else g.fillRect(x, y, bw, bar);
    }
    g.globalAlpha = 1;
    r.frame = requestAnimationFrame(draw);
  }

  function stopTake() {
    var r = rec;
    rec = null;
    clearInterval(r.timer);
    if (r.frame) cancelAnimationFrame(r.frame);
    box.classList.remove("hearing");
    return new Promise(function (done) {
      r.media.onstop = function () {
        r.stream.getTracks().forEach(function (t) { t.stop(); });
        if (r.ctx && r.ctx.close) r.ctx.close();
        done(r);
      };
      r.media.stop();
    });
  }

  function finish() {
    if (!rec) return;
    show([]);
    view("saving");
    stopTake().then(function (r) {
      line++;
      if (!r.chunks.length) {
        refused("Not saved", "Nothing was recorded that time.", "Check the microphone, then try again.", takes.length ? ["more"] : ["over"]);
        return;
      }
      takes.push(new Blob(r.chunks, { type: r.media.mimeType || mimeType }));
      recordedBefore += r.recorded;
      estimated = confirmed + r.speech;
      upload();
    });
  }

  function discard() {
    takes = [];
    confirmed = 0;
    estimated = 0;
    recordedBefore = 0;
    line = 0;
  }

  function restart() {
    var reset = function () {
      discard();
      close();
      setHeard(0);
      say.textContent = "Your recording was discarded. Start again whenever you’re ready.";
      startBtn.focus();
    };
    if (rec) stopTake().then(reset); else reset();
  }

  // The refused view: what happened, said plainly, and the one way on.
  function refused(kicker, title, text, actions) {
    open();
    view("refused");
    $("refused-kicker").textContent = kicker;
    $("refused-title").textContent = title;
    $("refused-text").textContent = text;
    show(actions);
    restartBtn.hidden = actions.indexOf("over") >= 0;
    hint.textContent = "";
    $("refused-title").focus();
  }

  // --- saving -----------------------------------------------------------------

  function upload() {
    var form = new FormData();
    var ext = /mp4/.test(mimeType) ? "m4a" : /ogg/.test(mimeType) ? "ogg" : "webm";
    takes.forEach(function (b, i) { form.append("take", b, "take-" + (i + 1) + "." + ext); });

    fetch("/enroll/ticket", {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": box.dataset.csrf }
    }).then(function (r) {
      if (r.status === 401) throw new Error("signed out");
      if (!r.ok) throw new Error("ticket");
      return r.json();
    }).then(function (t) {
      return fetch(box.dataset.voiceUrl, {
        method: "POST",
        headers: { "Authorization": "Bearer " + t.ticket },
        body: form
      }).catch(function () { throw new Error("unreachable"); });
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (body) { answer(r.status, body); });
    }).catch(function (err) {
      if (err.message === "signed out") {
        refused("Not saved", "Your sign-in has expired.", "Reload the page and sign in again; you’ll need to record again.", []);
        return;
      }
      refused("Not saved yet", err.message === "unreachable" ? "The voice service couldn’t be reached." : "Your recording couldn’t be saved.",
        "Your takes so far are kept on this page. Try again in a moment.", ["retry"]);
    });
  }

  function answer(status, body) {
    var heard = Number(body.voicedSeconds) || 0;
    if (status === 201) {
      discard();
      close();
      intro.hidden = true;
      saved.hidden = false;
      $("saved-seconds").textContent = Math.floor(heard) + " s";
      $("saved-replaced").hidden = !body.replaced;
      saved.querySelector("h1").focus();
      return;
    }
    if (status === 422) {
      if (Number(body.requiredSeconds) > 0) setMin(Number(body.requiredSeconds));
      switch (body.reason) {
        case "too_quiet":
          discard();
          refused("Not saved", heard > 0 ? "Your voice came through too quietly." : "The voice service couldn’t hear anything.",
            heard > 0 ? "Move closer to the microphone, or check it’s the one you meant to use, then start over."
              : "Check that the microphone isn’t muted and is the one you meant to use, then start over.", ["over"]);
          return;
        case "clipping":
          discard();
          refused("Not saved", "Your voice came through distorted.",
            "It sounds as if the microphone is too close or turned up too far. Move back a little, then start over.", ["over"]);
          return;
        default:
          confirmed = heard;
          refused("Not saved yet", "The voice service heard only " + seconds(heard) + " of speech.",
            "It needs at least " + Math.ceil(MIN) + ". Your takes so far are kept on this page, and nothing was saved anywhere else. " +
            (body.reason === "mostly_silence" ? "A lot of the recording was quiet; try to keep talking. " : "") +
            "Say a few more lines and finish again.", ["more"]);
          // The meter shows what the voice service counted, against what it
          // needs; this page's own estimate is set beside it.
          setFill(confirmed);
          left.textContent = "";
          var b = document.createElement("b");
          b.textContent = String(Math.floor(confirmed));
          var aside = document.createElement("span");
          aside.className = "aside";
          aside.textContent = " · " + Math.floor(estimated) + " s by this page";
          left.appendChild(b);
          left.appendChild(document.createTextNode(" s counted"));
          left.appendChild(aside);
          right.textContent = Math.ceil(MIN) + " s needed";
          hint.textContent = "This page’s count is a rough estimate; the voice service’s is the one that counts.";
          return;
      }
    }
    if (status === 401) {
      refused("Not saved yet", "That took a little too long to check.", "Your takes are still here. Try again.", ["retry"]);
      return;
    }
    if (status === 413 || status === 415) {
      discard();
      refused("Not saved", status === 413 ? "That’s more recording than the voice service accepts at once." : "The voice service couldn’t read this browser’s recording.",
        status === 413 ? "Please start over and finish a little sooner." : "Please start over, or try another browser.", ["over"]);
      return;
    }
    refused("Not saved yet", "Your recording couldn’t be saved.",
      (typeof body.error === "string" && status === 502) ? body.error : "Your takes are still here. Try again in a moment.", ["retry"]);
  }

  startBtn.addEventListener("click", start);
  buttons.more.addEventListener("click", start);
  buttons.next.addEventListener("click", nextLine);
  buttons.finish.addEventListener("click", finish);
  buttons.retry.addEventListener("click", function () { show([]); view("saving"); upload(); });
  buttons.over.addEventListener("click", restart);
  restartBtn.addEventListener("click", restart);
})();
