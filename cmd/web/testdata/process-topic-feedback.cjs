const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const options = vm.runInNewContext("(" + process.argv[2] + ")", {}, {timeout: 1000});
const feedback = {hidden: true};
const form = {querySelector(selector) {
  assert.equal(selector, "[data-process-feedback]");
  return feedback;
}};
const documentListeners = new Map();
const document = {
  hidden: false,
  addEventListener(name, handler) {documentListeners.set(name, handler);},
  removeEventListener(name) {documentListeners.delete(name);},
  dispatchEvent(event) {documentListeners.get(event.type)?.(event);},
};
let events = [];
// Datastar v1.0.2 dispatches this document event with the originating element.
// The rendered __document attribute is required by the Go regression.
const emit = (type, el, argsRaw = {}) => {
  events.push(type);
  document.dispatchEvent({type: "datastar-fetch", detail: {type, el, argsRaw}});
};
const context = vm.createContext({
  document, window: {}, AbortController, setTimeout, clearTimeout,
  re: emit, Tn: "retrying", An: "retries-failed",
  ae: value => value.replace(/[A-Z]/g, match => "-" + match.toLowerCase()),
});
vm.runInContext(fs.readFileSync(path.join(__dirname, "datastar-v1.0.2-fetch.cjs"), "utf8"), context, {timeout: 1000});
context.expression = process.argv[3];
const listener = vm.runInContext("new Function('el', 'evt', expression)", context, {timeout: 1000});
document.addEventListener("datastar-fetch", event => listener(form, event));

(async () => {
  for (const mode of ["network", "truncated-body", "http", "success"]) {
    events = [];
    // Only a new submission of this form clears its previous uncertainty.
    feedback.hidden = false;
    emit("started", form);
    assert.equal(feedback.hidden, true, "new submission must clear stale feedback");
    let calls = 0, failure = "";
    try {
      await context.Ln(form, () => ({
        ...options,
        input: "/process-topic", method: "POST", body: "topic=rust",
        openWhenHidden: true, retryInterval: 0,
        // This is the pinned post action's onopen callback: HTTP failures
        // emit error even though the fetch promise itself resolves.
        onopen: async response => {
          if (response.status >= 400) emit("error", form, {status: response.status.toString()});
        },
        fetch: async () => {
          calls++;
          if (mode === "network") throw new TypeError("connection lost");
          return {
            status: mode === "http" ? 503 : 200,
            headers: {get: name => name === "Content-Type" ? "text/html" : null},
            text: async () => {
              if (mode === "truncated-body") throw new TypeError("response body interrupted");
              return '<div id="topic-status">status response</div>';
            },
          };
        },
      }));
    } catch (error) {
      failure = String(error);
    } finally {
      emit("finished", form);
    }
    assert.equal(calls, 1, "feedback must not replay a POST");
    assert.equal(feedback.hidden, mode === "success", "finished must preserve failure feedback without showing it for success");
    assert.equal(Boolean(failure), mode === "network" || mode === "truncated-body");
    assert.equal(events.includes("datastar-patch-elements"), mode === "success");
    console.log(JSON.stringify({mode, calls, feedbackVisible: !feedback.hidden, events}));
  }
  for (const unrelated of [{}, {id: "topic-status"}]) {
    feedback.hidden = true;
    for (const type of ["error", "retries-failed", "finished"]) emit(type, unrelated);
    assert.equal(feedback.hidden, true, "unrelated form/status failures must not show this form's alert");
    feedback.hidden = false;
    emit("started", unrelated);
    emit("finished", unrelated);
    assert.equal(feedback.hidden, false, "unrelated polling must not clear this form's alert");
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
