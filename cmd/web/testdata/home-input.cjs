const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const script = fs.readFileSync(0, "utf8");

function setup(restoredValue = "") {
  const listeners = new Map();
  function element(id) {
    return {
      value: id === "topic-input" ? restoredValue : "",
      textContent: "", children: [], disabled: false, hidden: true,
      addEventListener(name, callback) { listeners.set(id + ":" + name, callback); },
      setAttribute() {}, removeAttribute() {}, appendChild(child) { this.children.push(child); },
      set innerHTML(value) { this.children = []; },
    };
  }
  const elements = Object.fromEntries(["topic-input", "topic-button", "topic-status", "topic-results", "topic-form"].map(id => [id, element(id)]));
  const document = { getElementById(id) { return elements[id]; }, createElement() { return element("option"); } };
  const window = { addEventListener(name, callback) { listeners.set("window:" + name, callback); }, setTimeout() {} };
  // Leave autocomplete pending: enable/disable must not wait for the network.
  const context = vm.createContext({document, window, AbortController, fetch() { return new Promise(() => {}); }});
  vm.runInContext(script, context);
  return { elements, listeners, run(code) { return vm.runInContext(code, context); } };
}

const {elements, listeners} = setup();
const input = elements["topic-input"], button = elements["topic-button"];
assert.equal(button.disabled, true, "initial empty input must disable submission");
for (const value of [" ", "\t ", "\u00a0"]) {
  input.value = value; listeners.get("topic-input:input")();
  assert.equal(button.disabled, true, "whitespace must stay disabled");
}
for (const value of ["Go", "R", "C++", "C#", ".NET", "  sqlite  "]) {
  input.value = value; listeners.get("topic-input:input")();
  assert.equal(button.disabled, false, "nonblank input must enable before autocomplete returns");
  input.value = ""; listeners.get("topic-input:input")();
  assert.equal(button.disabled, true, "clearing must disable immediately");
}
input.value = "  "; let prevented = false;
listeners.get("topic-form:submit")({ preventDefault() { prevented = true; } });
assert.equal(prevented, true, "blank keyboard submit must be prevented");
input.value = "Rust"; listeners.get("window:pageshow")();
assert.equal(button.disabled, false, "restored nonblank value must enable on pageshow");
input.value = ""; listeners.get("window:pageshow")();
assert.equal(button.disabled, true, "restored blank value must disable on pageshow");
assert.equal(setup("C++").elements["topic-button"].disabled, false, "initial restored value must work");
console.log("Actual home script passed: initial/whitespace disabled, immediate typing/clearing, short and punctuated names, submit guard, and restored values.");

const identity = setup("C");
identity.run('matches = [{slug: "c", name: "C++"}];');
assert.equal(identity.run('exactMatch()'), undefined, "legacy C++ slug cannot label C as an existing subject");
identity.elements["topic-input"].value = "C++";
assert.equal(identity.run('exactMatch().name'), "C++", "same-name legacy topic still matches");
identity.elements["topic-input"].value = "NET";
identity.run('matches = [{slug: "net", name: ".NET"}];');
assert.equal(identity.run('exactMatch()'), undefined, "legacy .NET slug cannot capture NET");
