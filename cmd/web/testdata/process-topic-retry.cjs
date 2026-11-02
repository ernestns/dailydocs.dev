const fs = require("node:fs");
const path = require("node:path");
const readline = require("node:readline");
const vm = require("node:vm");

// Execute only the pinned transport helper, with the actual rendered form's
// options. Requests go to the Go handler over pipes, never to a network socket.
const options = vm.runInNewContext("(" + process.argv[2] + ")", {}, {timeout: 1000});
if (process.argv[3] === "defaults") {
  delete options.retry;
  delete options.retryMaxCount;
} else if (process.argv[3] === "never-only") {
  options.retry = "never";
  delete options.retryMaxCount;
}
const replies = readline.createInterface({input: process.stdin});
const input = replies[Symbol.asyncIterator]();
const send = value => process.stdout.write(JSON.stringify(value) + "\n");
const context = vm.createContext({
  document: {hidden: false, addEventListener() {}, removeEventListener() {}},
  window: {}, AbortController, setTimeout, clearTimeout,
  re() {}, Tn: "retrying", An: "retries-failed",
  ae: value => value.replace(/[A-Z]/g, match => "-" + match.toLowerCase()),
});
vm.runInContext(fs.readFileSync(path.join(__dirname, "datastar-v1.0.2-fetch.cjs"), "utf8"), context, {timeout: 1000});

(async () => {
  let attempts = 0, error = "";
  try {
    await context.Ln({}, () => ({
      ...options,
      input: "/process-topic", method: "POST", body: "topic=rust",
      openWhenHidden: true,
      // Preserve retry decisions while avoiding a one-second wait per control.
      retryInterval: 0,
      fetch: async (url, request) => {
        send({type: "request", attempt: ++attempts, url, method: request.method, body: request.body});
        const reply = await input.next();
        if (reply.done) throw new Error("test handler disconnected");
        const response = JSON.parse(reply.value);
        if (response.drop) throw new TypeError("response lost after server accepted the request");
        return {
          status: response.status,
          headers: {get: name => name === "Content-Type" ? "text/html" : null},
          text: async () => response.body,
        };
      },
    }));
  } catch (failure) {
    error = String(failure);
  }
  send({type: "done", attempts, error});
  replies.close();
  process.stdin.destroy();
})().catch(error => {console.error(error); process.exitCode = 1;});
