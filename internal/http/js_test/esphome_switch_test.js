"use strict";
// Tests for content/static/js/esphome_switch.js, with a document and a request that are fakes. Run with: node js_test/esphome_switch_test.js
// (the Go test in js_test_test.go does).
var assert = require("assert");
var fs = require("fs");
var script = require("../content/static/js/esphome_switch.js");

var tests = [];
function test(name, fn) { tests.push([name, fn]); }

function El(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.textContent = "";
    this.value = "";
    this.parentNode = null;
    this.dataset = {};
    this.disabled = false;
}
El.prototype.appendChild = function (child) { child.parentNode = this; this.children.push(child); return child; };
El.prototype.removeChild = function (child) { this.children.splice(this.children.indexOf(child), 1); child.parentNode = null; };
El.prototype.setAttribute = function (k, v) { this.attrs[k] = v; };
El.prototype.addEventListener = function (type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); };
El.prototype.focus = function () { this.focused = true; };
El.prototype.fire = function (type, event) {
    var e = event || {};
    e.preventDefault = function () { e.prevented = true; };
    (this.listeners[type] || []).forEach(function (fn) { fn(e); });
    return e;
};
El.prototype.find = function (className) {
    if (this.className.split(" ").indexOf(className) >= 0) { return this; }
    for (var i = 0; i < this.children.length; i++) {
        var found = this.children[i].find(className);
        if (found) { return found; }
    }
    return null;
};

// A document whose <dialog> is a native one (has showModal) or is not, as in Safari 9.
function fakeDoc(nativeDialog) {
    var doc = {body: new El("body")};
    doc.createElement = function (tag) {
        var el = new El(tag);
        if (tag === "dialog" && nativeDialog) {
            el.open = false;
            el.showModalCalls = 0;
            el.closeCalls = 0;
            el.showModal = function () { el.open = true; el.showModalCalls++; };
            el.close = function () { el.open = false; el.closeCalls++; el.fire("close"); };
        }
        return el;
    };
    return doc;
}

function ask(doc, options) {
    var answers = [];
    new script.ConfirmDialog(doc).ask(options, function (a) { answers.push(a); });
    return answers;
}

test("with no <dialog> the question is asked in a box over the page, and a password is taken from it", function () {
    var doc = fakeDoc(false);
    var answers = ask(doc, {message: "Turn off Rack?", askPassword: true});
    var layer = doc.body.children[0];

    assert.strictEqual(layer.tagName, "div");
    assert.strictEqual(layer.className, "esphome-confirm-overlay");
    assert.strictEqual(layer.find("esphome-confirm").attrs.role, "dialog");
    assert.strictEqual(layer.find("message").textContent, "Turn off Rack?");

    var input = layer.find("password");
    assert.strictEqual(input.focused, true);
    input.value = "hovno";
    layer.find("esphome-confirm").children[0].fire("submit");

    assert.deepStrictEqual(answers, [{confirmed: true, password: "hovno"}]);
    assert.strictEqual(input.value, "");
    assert.strictEqual(doc.body.children.length, 0);
});

test("an empty password is not taken, where the browser does not check it", function () {
    var doc = fakeDoc(false);
    var answers = ask(doc, {message: "m", askPassword: true});
    var form = doc.body.children[0].find("esphome-confirm").children[0];

    var event = form.fire("submit");

    assert.strictEqual(event.prevented, true);
    assert.deepStrictEqual(answers, []);
    assert.strictEqual(doc.body.children.length, 1);
});

test("cancelling and Escape say no, and only once", function () {
    var doc = fakeDoc(false);
    var answers = ask(doc, {message: "m", askPassword: false});
    doc.body.children[0].find("cancel").fire("click");
    doc.body.children[0] && doc.body.children[0].fire("keydown", {key: "Escape"});
    assert.deepStrictEqual(answers, [{confirmed: false, password: ""}]);
    assert.strictEqual(doc.body.children.length, 0);

    doc = fakeDoc(false);
    answers = ask(doc, {message: "m", askPassword: false});
    doc.body.children[0].fire("keydown", {keyCode: 27});
    doc.body.children[0] && doc.body.children[0].fire("keydown", {keyCode: 27});
    assert.deepStrictEqual(answers, [{confirmed: false, password: ""}]);
});

test("a switch with no password is confirmed by the button and takes the focus", function () {
    var doc = fakeDoc(false);
    var answers = ask(doc, {message: "m", askPassword: false});
    var layer = doc.body.children[0];

    assert.strictEqual(layer.find("confirm").focused, true);
    assert.strictEqual(layer.find("password"), null);
    layer.find("esphome-confirm").children[0].fire("submit");
    assert.deepStrictEqual(answers, [{confirmed: true, password: ""}]);
});

test("a browser that has <dialog> uses it, and closing it says no", function () {
    var doc = fakeDoc(true);
    var answers = ask(doc, {message: "m", askPassword: false});
    var dialog = doc.body.children[0];

    assert.strictEqual(dialog.tagName, "dialog");
    assert.strictEqual(dialog.className, "esphome-confirm");
    assert.strictEqual(dialog.showModalCalls, 1);

    // Escape: the browser closes the dialog.
    dialog.close();
    assert.deepStrictEqual(answers, [{confirmed: false, password: ""}]);
    assert.strictEqual(doc.body.children.length, 0);
});

test("a browser that has <dialog> closes it when it is confirmed, once", function () {
    var doc = fakeDoc(true);
    var answers = ask(doc, {message: "m", askPassword: false});
    var dialog = doc.body.children[0];

    dialog.children[0].fire("submit");

    assert.deepStrictEqual(answers, [{confirmed: true, password: ""}]);
    assert.strictEqual(dialog.closeCalls, 1);
});

// A card of a switch with two buttons, and the page with it.
function fakePage(control) {
    var card = new El("div");
    card.dataset = {device: "plug1", object: "relay", name: "Rack"};
    var error = new El("div");
    var on = new El("button");
    on.dataset = {on: "true", control: control};
    var off = new El("button");
    off.dataset = {on: "false", control: control};
    card.querySelector = function () { return error; };
    card.querySelectorAll = function () { return [on, off]; };

    var doc = {querySelectorAll: function () { return [card]; }};

    return {card: card, error: error, on: on, off: off, doc: doc};
}

function switches(page, overrides) {
    var posts = [];
    var reloads = [];
    var options = {
        post: function (url, body, done) { posts.push({url: url, body: body, done: done}); },
        later: function (fn, ms) { reloads.push({fn: fn, ms: ms}); },
        reload: function () {},
        ui: {ask: function () { throw new Error("asked"); }}
    };
    Object.keys(overrides || {}).forEach(function (k) { options[k] = overrides[k]; });

    var s = new script.EsphomeSwitches(page.doc, options);
    assert.strictEqual(s.init(), true);

    return {s: s, posts: posts, reloads: reloads};
}

test("a click posts the switch and, once it has been sent, loads the page again a second later", function () {
    var page = fakePage("open");
    var t = switches(page);

    page.off.fire("click");

    assert.strictEqual(t.posts.length, 1);
    assert.strictEqual(t.posts[0].url, "/plugins/esphome/switch");
    assert.deepStrictEqual(t.posts[0].body, {Device: "plug1", Object: "relay", On: false});
    assert.strictEqual(page.on.disabled && page.off.disabled, true);

    t.posts[0].done(null, 200, "");

    assert.strictEqual(t.reloads.length, 1);
    assert.strictEqual(t.reloads[0].ms, 1000);
});

test("a refusal is said on the page, in the server's words, and the buttons come back", function () {
    var page = fakePage("open");
    var t = switches(page);

    page.on.fire("click");
    t.posts[0].done(null, 403, "Failed to handle request: wrong password\nmore about it");

    assert.strictEqual(page.error.textContent, "Could not send the command (403): wrong password");
    assert.strictEqual(page.on.disabled || page.off.disabled, false);
    assert.strictEqual(t.reloads.length, 0);
});

test("no answer at all is said too", function () {
    var page = fakePage("open");
    var t = switches(page);

    page.on.fire("click");
    t.posts[0].done(new Error("the request could not be sent"));

    assert.ok(page.error.textContent.indexOf("Could not send the command: ") === 0);
    assert.strictEqual(page.on.disabled, false);
});

test("a switch that wants confirmation asks first, and nothing is sent if the answer is no", function () {
    var page = fakePage("confirm");
    var asked = [];
    var t = switches(page, {ui: {ask: function (o, done) { asked.push(o); done({confirmed: false, password: ""}); }}});

    page.on.fire("click");

    assert.deepStrictEqual(asked, [{message: "Turn on Rack?", askPassword: false}]);
    assert.strictEqual(t.posts.length, 0);
    assert.strictEqual(page.on.disabled, false);
});

test("a confirmed switch is sent as confirmed, and a password one with its password", function () {
    var page = fakePage("confirm");
    var t = switches(page, {ui: {ask: function (o, done) { done({confirmed: true, password: "ignored"}); }}});
    page.off.fire("click");
    assert.deepStrictEqual(t.posts[0].body, {Device: "plug1", Object: "relay", On: false, Confirmed: true});

    page = fakePage("password");
    t = switches(page, {ui: {ask: function (o, done) { assert.strictEqual(o.askPassword, true); done({confirmed: true, password: "hovno"}); }}});
    page.on.fire("click");
    assert.deepStrictEqual(t.posts[0].body, {Device: "plug1", Object: "relay", On: true, Confirmed: true, Password: "hovno"});
});

test("init says whether there was a switch on the page", function () {
    var s = new script.EsphomeSwitches({querySelectorAll: function () { return []; }}, {ui: {}});
    assert.strictEqual(s.init(), false);
});

// The script has to run on Safari 9. Nothing here can run it there, so what it cannot parse or call is looked for in the source.
test("the script uses nothing Safari 9 does not have", function () {
    var source = fs.readFileSync(__dirname + "/../content/static/js/esphome_switch.js", "utf8")
        .replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

    [
        [/=>/, "an arrow function"], [/\bclass\s+\w/, "a class"], [/\blet\s/, "let"], [/\bconst\s/, "const"], [/`/, "a template literal"],
        [/\basync\b/, "async"], [/\bawait\b/, "await"], [/\bfetch\s*\(/, "fetch"], [/\.\.\./, "spread"], [/\?\./, "optional chaining"],
        [/\?\?/, "nullish coalescing"], [/\bObject\.(entries|values)\b/, "Object.entries or values"], [/\.padStart\b|\.padEnd\b/, "padStart"],
        [/\}\s*=\s*\w|\{\s*\w+\s*,\s*\w+\s*\}\s*=/, "destructuring"]
    ].forEach(function (pair) {
        assert.ok(!pair[0].test(source), "the script has " + pair[1]);
    });

    // forEach is only for arrays: a NodeList has none before Safari 10, which is what toArray is for.
    (source.match(/(\w+)\.forEach/g) || []).forEach(function (use) {
        assert.ok(use === "cards.forEach" || use === "buttons.forEach", "forEach on " + use);
    });
});

var failed = 0;
tests.forEach(function (t) {
    try { t[1](); console.log("ok   " + t[0]); } catch (e) { failed++; console.log("FAIL " + t[0] + "\n     " + (e && e.stack || e)); }
});
if (failed) { process.exit(1); }
