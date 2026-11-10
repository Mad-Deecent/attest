// Guided voice recording. The person says short lines one at a time while a
// rough, in-browser estimate of how long they have been speaking fills a
// meter. Each stretch of recording is a "take"; on Finish every take so far
// goes straight to the voice service with a fresh single-use ticket from
// attest. The voice service decides what counts as speech and answers with
// how much it heard. If that is not enough, the person keeps going and the
// next save sends the earlier takes again with the new one; nothing is kept
// between saves anywhere but in this page. attest never receives audio.
(function () {
  "use strict";
  var box = document.getElementById("recorder");
  if (!box) return;

  var $ = function (id) { return document.getElementById(id); };
  var say = $("voice-status");
  var stage = $("stage");
  var promptEl = $("prompt");
  var kindEl = $("prompt-kind");
  var countEl = $("prompt-count");
  var meter = $("meter");
  var fill = $("meter-fill");
  var heardEl = $("heard");
  var canvas = $("trace");
  var buttons = {
    start: $("start"), next: $("next"), finish: $("finish"),
    more: $("more"), retry: $("retry"), restart: $("restart")
  };
  var prompts = Array.prototype.map.call($("prompt-list").children, function (li) {
    return { text: li.textContent, read: li.hasAttribute("data-read") };
  });
  var TARGET = Number(box.dataset.target) || 30;
  var MAX_RECORDING = 100; // seconds across all takes; the voice service takes up to 120
  var STEP = 0.05;         // seconds per level reading
  var reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  box.hidden = false;

  if (!window.MediaRecorder || !navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    buttons.start.disabled = true;
    say.textContent = "This browser can't record audio here. You can skip this step, or come back in another browser.";
    return;
  }

  // Chrome and Firefox record webm/opus; Safari records mp4 (AAC).
  var mimeType = ["audio/webm;codecs=opus", "audio/mp4", "audio/ogg;codecs=opus", "audio/webm"].filter(function (t) {
    return MediaRecorder.isTypeSupported && MediaRecorder.isTypeSupported(t);
  })[0] || "";

  var takes = [];        // Blobs, oldest first
  var confirmed = 0;     // seconds of speech the voice service has counted in `takes`
  var recordedBefore = 0;// seconds of recording in `takes`
  var line = 0;          // index into prompts
  var rec = null;        // the current take, while recording
  var milestone = 0;

  function show(names) {
    Object.keys(buttons).forEach(function (k) { buttons[k].hidden = names.indexOf(k) < 0; });
  }

  function seconds(n) { n = Math.floor(n); return n + (n === 1 ? " second" : " seconds"); }

  // --- the meter ------------------------------------------------------------

  function setHeard(total) {
    var shown = Math.min(Math.floor(total), TARGET);
    heardEl.textContent = String(Math.floor(total));
    fill.style.transform = "scaleX(" + Math.min(total / TARGET, 1) + ")";
    meter.setAttribute("aria-valuenow", String(shown));
    meter.setAttribute("aria-valuetext", seconds(total) + " of speech, of about " + TARGET);
    // Announce a few milestones, not every second.
    var marks = [[TARGET / 2, "Halfway there."], [TARGET, "That's plenty. Finish whenever you like."]];
    for (var i = milestone; i < marks.length && total >= marks[i][0]; i++) {
      say.textContent = marks[i][1];
      milestone = i + 1;
    }
  }

  function showLine() {
    var p = prompts[line % prompts.length];
    countEl.textContent = "Line " + (line % prompts.length + 1) + " of " + prompts.length;
    kindEl.textContent = p.read ? "Read aloud" : "Say it naturally";
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
      say.textContent = "The microphone isn't available. Check that this page may use it, then try again.";
      show(takes.length ? ["more", "restart"] : ["start"]);
    });
  }

  function begin(stream) {
    var r = {
      stream: stream, chunks: [], speech: 0, recorded: 0,
      lineSpeech: 0, quiet: 0, floor: 0, levels: []
    };
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

    stage.hidden = false;
    stage.classList.remove("paused");
    showLine();
    setHeard(confirmed);
    show(["next", "finish", "restart"]);
    stage.scrollIntoView({ block: "nearest", behavior: reduceMotion ? "auto" : "smooth" });
    say.textContent = "Recording. Say the line, and the next one appears when you pause.";
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
    if (reduceMotion) stage.classList.toggle("hearing", speaking);

    if (speaking) {
      r.speech += STEP;
      r.lineSpeech += STEP;
      r.quiet = 0;
    } else {
      r.quiet += STEP;
    }
    // A line is done after some speech and then a pause.
    if (r.lineSpeech >= 1.2 && r.quiet >= 0.9) {
      var total = confirmed + r.speech;
      if (total >= TARGET) return finish();
      nextLine();
    }
    setHeard(confirmed + r.speech);
    if (recordedBefore + r.recorded >= MAX_RECORDING) finish();
  }

  function nextLine() {
    line++;
    if (rec) { rec.lineSpeech = 0; rec.quiet = 0; }
    showLine();
  }

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
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, w, h);
    var gap = 3, n = Math.floor(w / gap);
    var data = r.levels.slice(-n);
    var x0 = w - data.length * gap;
    for (var i = 0; i < data.length; i++) {
      var d = data[i];
      var amp = Math.min(1, Math.sqrt(d.level / 0.25));
      var bar = Math.max(1, amp * (h - 2));
      g.fillStyle = d.speaking ? css.getPropertyValue("--live") : css.getPropertyValue("--faint");
      g.fillRect(x0 + i * gap, (h - bar) / 2, 1.5, bar);
    }
    r.frame = requestAnimationFrame(draw);
  }

  function stopTake() {
    var r = rec;
    rec = null;
    clearInterval(r.timer);
    if (r.frame) cancelAnimationFrame(r.frame);
    stage.classList.remove("hearing");
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
    say.textContent = "Checking your recording.";
    stopTake().then(function (r) {
      stage.hidden = true;
      canvas.getContext("2d").clearRect(0, 0, canvas.width, canvas.height);
      line++;
      if (!r.chunks.length) {
        say.textContent = "Nothing was recorded that time. Check the microphone, then try again.";
        show(takes.length ? ["more", "restart"] : ["start"]);
        return;
      }
      takes.push(new Blob(r.chunks, { type: r.media.mimeType || mimeType }));
      recordedBefore += r.recorded;
      upload();
    });
  }

  function restart() {
    var reset = function () {
      takes = [];
      confirmed = 0;
      recordedBefore = 0;
      milestone = 0;
      line = 0;
      stage.hidden = true;
      setHeard(0);
      say.textContent = "Your recording was discarded. Start again whenever you're ready.";
      show(["start"]);
      buttons.start.focus();
    };
    if (rec) stopTake().then(reset); else reset();
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
        say.textContent = "Your sign-in has expired. Reload the page and sign in again; you'll need to record again.";
        show([]);
        return;
      }
      say.textContent = err.message === "unreachable"
        ? "The voice service couldn't be reached. Your recording is still here: try again in a moment."
        : "Your recording couldn't be saved. It's still here: try again in a moment.";
      show(["retry", "restart"]);
      buttons.retry.focus();
    });
  }

  function answer(status, body) {
    var heard = Number(body.voicedSeconds) || 0;
    if (status === 201) {
      takes = [];
      confirmed = 0;
      recordedBefore = 0;
      milestone = 0;
      line = 0;
      var msg = "Done. Your voice is saved: we heard " + seconds(heard) + " of you speaking.";
      if (body.replaced) msg += " It replaces the one you saved before.";
      say.textContent = msg + " You can go on to the next step.";
      buttons.start.textContent = "Record again";
      show(["start"]);
      markDone();
      return;
    }
    if (status === 422) {
      confirmed = heard;
      milestone = heard >= TARGET ? 2 : heard >= TARGET / 2 ? 1 : 0;
      setHeard(confirmed);
      var need = Math.max(1, Math.ceil((Number(body.requiredSeconds) || TARGET) - Math.floor(heard)));
      switch (body.reason) {
        case "too_quiet":
          takes = [];
          say.textContent = heard > 0
            ? "Your voice came through too quietly. Move closer to the microphone, or check it's the one you meant to use, then start over."
            : "We couldn't hear anything. Check that the microphone isn't muted and is the one you meant to use, then start over.";
          show(["restart"]);
          buttons.restart.focus();
          return;
        case "clipping":
          takes = [];
          say.textContent = "Your voice came through distorted, as if the microphone were too close or turned up too far. Move back a little, then start over.";
          show(["restart"]);
          buttons.restart.focus();
          return;
        default:
          // Keep the meter in view at what the voice service counted, with
          // the line to say next.
          showLine();
          countEl.textContent = "Paused";
          kindEl.textContent = "Next up";
          stage.classList.add("paused");
          stage.hidden = false;
          say.textContent = "Almost there. We heard " + seconds(heard) + " of speech and need at least " +
            seconds(Number(body.requiredSeconds) || TARGET) + ", so about " + seconds(need) + " more. " +
            (body.reason === "mostly_silence" ? "A lot of the recording was quiet; try to keep talking. " : "") +
            "Keep going and we'll add it to what you've already said.";
          show(["more", "restart"]);
          buttons.more.focus();
          return;
      }
    }
    if (status === 401) {
      say.textContent = "That took a little too long to check. Your recording is still here: try again.";
      show(["retry", "restart"]);
      buttons.retry.focus();
      return;
    }
    if (status === 413 || status === 415) {
      takes = [];
      say.textContent = status === 413
        ? "That's more recording than the voice service accepts at once. Please start over and finish a little sooner."
        : "The voice service couldn't read this browser's recording. Please start over, or try another browser.";
      show(["restart"]);
      buttons.restart.focus();
      return;
    }
    say.textContent = (typeof body.error === "string" && status === 502)
      ? body.error
      : "Your recording couldn't be saved. It's still here: try again in a moment.";
    show(["retry", "restart"]);
    buttons.retry.focus();
  }

  function markDone() {
    var step = $("voice-step");
    if (step.classList.contains("done")) return;
    step.classList.add("done");
    var tag = document.createElement("span");
    tag.className = "tag";
    tag.textContent = "Done";
    var h2 = step.querySelector("h2");
    h2.appendChild(document.createTextNode(" "));
    h2.appendChild(tag);
    if (step.getAttribute("aria-current")) {
      step.removeAttribute("aria-current");
      if (step.nextElementSibling) step.nextElementSibling.setAttribute("aria-current", "step");
    }
  }

  buttons.start.addEventListener("click", start);
  buttons.more.addEventListener("click", start);
  buttons.next.addEventListener("click", nextLine);
  buttons.finish.addEventListener("click", finish);
  buttons.retry.addEventListener("click", function () { show([]); say.textContent = "Checking your recording."; upload(); });
  buttons.restart.addEventListener("click", restart);
})();
