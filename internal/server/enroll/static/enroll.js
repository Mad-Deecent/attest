// Records a voice sample in the browser and sends it straight to the voice
// service. attest only issues the single-use ticket that says who it is for.
(function () {
  "use strict";
  var box = document.getElementById("recorder");
  if (!box) return;
  var say = document.getElementById("voice-status");
  var start = document.getElementById("start");
  var stop = document.getElementById("stop");
  box.hidden = false;

  if (!window.MediaRecorder || !navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    start.disabled = true;
    say.textContent = "This browser can't record audio here. You can skip this step.";
    return;
  }

  var recorder = null;
  var chunks = [];

  start.addEventListener("click", function () {
    start.disabled = true;
    navigator.mediaDevices.getUserMedia({ audio: true }).then(function (stream) {
      chunks = [];
      recorder = new MediaRecorder(stream);
      recorder.ondataavailable = function (e) { if (e.data.size) chunks.push(e.data); };
      recorder.onstop = function () {
        stream.getTracks().forEach(function (t) { t.stop(); });
        upload(new Blob(chunks, { type: recorder.mimeType }));
      };
      recorder.start();
      stop.disabled = false;
      stop.focus();
      say.textContent = "Recording. Read the sentence aloud, then press Stop and save.";
    }, function () {
      start.disabled = false;
      say.textContent = "The microphone isn't available. Check that this page may use it, then try again.";
    });
  });

  stop.addEventListener("click", function () {
    stop.disabled = true;
    say.textContent = "Saving your recording.";
    recorder.stop();
  });

  function upload(blob) {
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
        headers: {
          "Authorization": "Bearer " + t.ticket,
          "Content-Type": blob.type || "application/octet-stream"
        },
        body: blob
      });
    }).then(function (r) {
      if (!r.ok) throw new Error("voice");
      say.textContent = "Your voice is saved. You can go on to the next step.";
      markDone();
    }).catch(function (err) {
      say.textContent = err.message === "signed out"
        ? "Your sign-in has expired. Reload the page and sign in again."
        : "Your recording couldn't be saved. Please try again.";
    }).finally(function () {
      start.disabled = false;
    });
  }

  function markDone() {
    var step = document.getElementById("voice-step");
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
})();
