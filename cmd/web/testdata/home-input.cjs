const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const script = fs.readFileSync(0, "utf8");

function setup(restoredValue = "", suggestions = undefined) {
  const requests = [];
  const listeners = new Map();
  function element(id) {
    return {
      attributes: new Map(),
      value: id === "topic-input" ? restoredValue : "",
      textContent: "", children: [], disabled: false, hidden: true,
      addEventListener(name, callback) { listeners.set(id + ":" + name, callback); },
      setAttribute(name, value) { this.attributes.set(name, value); },
      getAttribute(name) { return this.attributes.get(name) ?? null; },
      removeAttribute(name) { this.attributes.delete(name); },
      appendChild(child) { this.children.push(child); },
      set innerHTML(value) { this.children = []; },
    };
  }
  const elements = Object.fromEntries(["topic-input", "topic-button", "topic-status", "topic-results", "topic-form"].map(id => [id, element(id)]));
  const document = { getElementById(id) { return elements[id]; }, createElement() { return element("option"); } };
  const window = { addEventListener(name, callback) { listeners.set("window:" + name, callback); }, setTimeout() {} };
  // Leave autocomplete pending: enable/disable must not wait for the network.
  const context = vm.createContext({document, window, AbortController, fetch(path) {
    requests.push(path);
    return suggestions === undefined ? new Promise(() => {}) : Promise.resolve({ok: true, async json() {
      return typeof suggestions === "function" ? suggestions(path) : suggestions;
    }});
  }});
  vm.runInContext(script, context);
  return { elements, listeners, requests };
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

async function testAutocomplete() {
  const keyboard = setup("G", [{slug: "go", name: "Go"}, {slug: "godot", name: "Godot"}]);
  const input = keyboard.elements["topic-input"], results = keyboard.elements["topic-results"];
  const button = keyboard.elements["topic-button"];
  const key = (value) => {
    let prevented = false;
    keyboard.listeners.get("topic-input:keydown")({key: value, preventDefault() { prevented = true; }});
    return prevented;
  };
  const selected = (index) => {
    assert.equal(input.getAttribute("aria-activedescendant"), results.children[index].id);
    results.children.forEach((option, i) => assert.equal(option.getAttribute("aria-selected"), i === index ? "true" : "false"));
  };
  await keyboard.listeners.get("topic-input:input")();
  assert.equal(results.hidden, false, "autocomplete response must open options");
  assert.equal(input.getAttribute("aria-expanded"), "true");
  assert.deepEqual(results.children.map(option => option.textContent), ["Go", "Godot"]);
  assert.equal(key("ArrowDown"), true, "navigation must prevent default scrolling");
  selected(0);
  key("ArrowDown"); selected(1);
  key("ArrowDown"); selected(1);
  key("ArrowUp"); selected(0);
  key("ArrowUp"); selected(0);
  assert.equal(key("Enter"), true, "selecting an option must prevent form submission");
  assert.equal(input.value, "Go");
  assert.equal(button.textContent, "View Reading");
  assert.equal(results.hidden, true);
  assert.equal(input.getAttribute("aria-expanded"), "false");
  assert.equal(input.getAttribute("aria-activedescendant"), null);
  assert.equal(results.children.every(option => option.getAttribute("aria-selected") === "false"), true);
  input.value = "G";
  await keyboard.listeners.get("topic-input:input")();
  key("ArrowDown"); selected(0);
  key("Escape");
  assert.equal(results.hidden, true);
  assert.equal(input.value, "G", "Escape must dismiss without choosing an option");
  assert.equal(input.getAttribute("aria-activedescendant"), null);
  key("ArrowDown");
  assert.equal(results.hidden, false, "ArrowDown must reopen the options");
  selected(0);

  for (const [plain, name, slug] of [["C", "C++", "c"], ["NET", ".NET", "net"]]) {
    const identity = setup(plain, [{slug, name}]);
    await identity.listeners.get("topic-input:input")();
    assert.equal(identity.elements["topic-button"].textContent, "Request Topic", "legacy slug must not capture the typed name");
    identity.elements["topic-input"].value = name;
    await identity.listeners.get("topic-input:input")();
    assert.equal(identity.elements["topic-button"].textContent, "View Reading", "same-name legacy topic must match");
  }
  const endpointResponses = JSON.parse(process.argv[2]);
  for (const empty of [endpointResponses[0], null, []]) {
    const responses = [empty, endpointResponses[1]];
    const recovery = setup("not-known", () => responses.shift());
    const input = recovery.elements["topic-input"], button = recovery.elements["topic-button"];
    const results = recovery.elements["topic-results"];
    await recovery.listeners.get("topic-input:input")();
    assert.equal(button.disabled, false);
    assert.equal(button.textContent, "Request Topic");
    assert.equal(recovery.elements["topic-status"].textContent, "No matching topic found.");
    assert.equal(results.hidden, true);
    assert.equal(results.children.length, 0);
    assert.equal(input.getAttribute("aria-expanded"), "false");
    input.value = "Go";
    await recovery.listeners.get("topic-input:input")();
    assert.deepEqual(recovery.requests, ["/topics/search?q=not-known", "/topics/search?q=Go"], "editing after no match must fetch again without clearing");
    assert.equal(responses.length, 0);
    assert.equal(results.hidden, false);
    assert.equal(results.children[0].textContent, "Go");
    const keys = [];
    for (const key of ["ArrowDown", "Enter"]) {
      recovery.listeners.get("topic-input:keydown")({key, preventDefault() { keys.push(key); }});
      if (key === "ArrowDown") {
        assert.equal(input.getAttribute("aria-activedescendant"), results.children[0].id);
        assert.equal(results.children[0].getAttribute("aria-selected"), "true");
      }
    }
    assert.deepEqual(keys, ["ArrowDown", "Enter"]);
    assert.equal(input.value, "Go");
    assert.equal(button.textContent, "View Reading");
    assert.equal(results.hidden, true);
    input.value = "";
    await recovery.listeners.get("topic-input:input")();
    assert.equal(button.disabled, true);
  }
  console.log("Actual home script passed: network options, ArrowDown/ArrowUp bounds, Enter selection, Escape dismissal, accessible active state, colliding names, and null/empty response recovery with real search responses.");
}

testAutocomplete().catch(error => { console.error(error); process.exitCode = 1; });
