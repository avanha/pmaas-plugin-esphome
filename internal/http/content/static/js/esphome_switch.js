/*
 * The buttons of the switches on the ESPHome page: a click asks the server to turn the switch on or off, and asks the person first, and
 * for a password, if the switch needs it. A device reports its new state a moment after it has been told, so the page loads again to show it.
 *
 * It runs on old browsers, an iPad that no longer gets updates among them (Safari 9): plain ES5, with no arrow functions, classes, class
 * fields, let or const, destructuring, async functions or fetch, none of which it has, and a script that uses one of them does not
 * run at all, which is a page of switches that cannot be clicked. Nor does it have NodeList.forEach, nor the <dialog> element, so
 * the question is asked in a box of its own there (see the CSS). Nothing here is slower or worse in a modern browser.
 *
 * The parts are given what they use from outside (the document, how a request is made, how the page is reloaded), so that
 * they are tested with fakes: see js_test/esphome_switch_test.js.
 */
(function (global) {
    "use strict";

    // A NodeList from the old browsers has no forEach.
    function toArray(list) {
        return Array.prototype.slice.call(list);
    }

    // Asks the person at the page to confirm, and for a password if the switch needs one. It is a dialog of its own, not the browser's
    // prompt(), because that shows what is typed. It is built from elements, never from markup, so nothing in a name can become one.
    function ConfirmDialog(doc) {
        this.doc = doc;
    }

    // Calls done with {confirmed, password}. Cancelling, pressing Escape and closing it all give confirmed: false.
    ConfirmDialog.prototype.ask = function (options, done) {
        var doc = this.doc;
        var dialog = doc.createElement("dialog");
        var native = typeof dialog.showModal === "function";
        var finished = false;
        var input = null;

        // The container of what is built: the <dialog> itself, or a layer over the page with a box in it where there is no <dialog>.
        var layer = dialog;
        var box = dialog;

        if (!native) {
            layer = doc.createElement("div");
            layer.className = "esphome-confirm-overlay";
            box = doc.createElement("div");
            box.className = "esphome-confirm";
            box.setAttribute("role", "dialog");
            box.setAttribute("aria-modal", "true");
            layer.appendChild(box);
        } else {
            dialog.className = "esphome-confirm";
        }

        var form = doc.createElement("form");

        var text = doc.createElement("p");
        text.className = "message";
        text.textContent = options.message;
        form.appendChild(text);

        if (options.askPassword) {
            input = doc.createElement("input");
            input.type = "password";
            input.name = "password";
            input.className = "password";
            input.autocomplete = "off";
            input.required = true;
            input.setAttribute("aria-label", "Password");
            form.appendChild(input);
        }

        var buttons = doc.createElement("div");
        buttons.className = "buttons";

        var cancel = doc.createElement("button");
        cancel.type = "button";
        cancel.className = "cancel";
        cancel.textContent = "Cancel";

        var confirm = doc.createElement("button");
        confirm.type = "submit";
        confirm.className = "confirm";
        confirm.textContent = "Confirm";

        buttons.appendChild(cancel);
        buttons.appendChild(confirm);
        form.appendChild(buttons);
        box.appendChild(form);

        function finish(confirmed) {
            if (finished) {
                return;
            }

            finished = true;

            var password = input ? input.value : "";

            // Don't leave what was typed in the page.
            if (input) {
                input.value = "";
            }

            if (native && dialog.open) {
                dialog.close();
            }

            if (layer.parentNode) {
                layer.parentNode.removeChild(layer);
            }

            done({confirmed: confirmed, password: password});
        }

        cancel.addEventListener("click", function () {
            finish(false);
        });

        // A form that is submitted is confirmed, once there is a password where one is needed. A modern browser has already seen to that
        // (the input is required); Safari before 10.1 does not check, and would send nothing as the password.
        form.addEventListener("submit", function (event) {
            event.preventDefault();

            if (input && input.value === "") {
                input.focus();
                return;
            }

            finish(true);
        });

        // Escape closes a <dialog> by itself, and is told to this as the dialog closing. Elsewhere there is nothing to do it.
        layer.addEventListener("keydown", function (event) {
            if (event.key === "Escape" || event.key === "Esc" || event.keyCode === 27) {
                finish(false);
            }
        });

        if (native) {
            dialog.addEventListener("close", function () {
                finish(false);
            });
        }

        doc.body.appendChild(layer);

        if (native) {
            dialog.showModal();
        }

        (input || confirm).focus();
    };

    // Says a request: it posts the body as JSON, and calls done(error, status, text), error being set if there was no answer at all.
    function postJson(url, body, done) {
        var request = new global.XMLHttpRequest();

        request.open("POST", url, true);
        request.setRequestHeader("Content-Type", "application/json");

        request.onload = function () {
            done(null, request.status, request.responseText);
        };

        request.onerror = function () {
            done(new Error("the request could not be sent"));
        };

        request.send(JSON.stringify(body));
    }

    // What the server says about a request it refused is about the request ("wrong password", "the device is offline"), so it is worth showing:
    // its first line, without the prefix the server puts on it.
    function reasonFrom(text) {
        return String(text).replace(/^Failed to handle request: /, "").split("\n")[0].slice(0, 200);
    }

    function EsphomeSwitches(doc, options) {
        options = options || {};
        this.doc = doc;
        this.ui = options.ui || new ConfirmDialog(doc);
        this.post = options.post || postJson;
        this.reload = options.reload || function () {
            global.location.reload();
        };
        this.later = options.later || global.setTimeout.bind(global);
    }

    EsphomeSwitches.prototype.init = function () {
        var self = this;
        var cards = toArray(this.doc.querySelectorAll("div.entity-esphome-switch"));

        cards.forEach(function (card) {
            toArray(card.querySelectorAll("button.switch-toggle")).forEach(function (button) {
                button.addEventListener("click", function () {
                    self.send(card, button);
                });
            });
        });

        return cards.length > 0;
    };

    EsphomeSwitches.prototype.send = function (card, button) {
        var self = this;
        var errorElement = card.querySelector(".switch-error");
        var buttons = toArray(card.querySelectorAll("button.switch-toggle"));
        errorElement.textContent = "";

        var on = button.dataset.on === "true";
        var control = button.dataset.control || "open";
        var body = {Device: card.dataset.device, Object: card.dataset.object, On: on};

        function setDisabled(disabled) {
            buttons.forEach(function (b) {
                b.disabled = disabled;
            });
        }

        function sendIt() {
            setDisabled(true);

            self.post("/plugins/esphome/switch", body, function (error, status, text) {
                if (error) {
                    console.error("Failed to switch:", error);
                    errorElement.textContent = "Could not send the command: " + error;
                    setDisabled(false);
                    return;
                }

                if (status < 200 || status > 299) {
                    // Say so on the page: a click that silently does nothing is worse than an error.
                    var reason = reasonFrom(text);
                    console.error("Failed to switch:", status, reason);
                    errorElement.textContent = "Could not send the command (" + status + "): " + reason;
                    setDisabled(false);
                    return;
                }

                // The request has been sent, not carried out: the device reports its new state a moment later, so give it a second before
                // the page asks for it.
                self.later(self.reload, 1000);
            });
        }

        // The server decides what is allowed: this only asks, so that what it needs is there to be checked.
        if (control === "confirm" || control === "password") {
            var name = card.dataset.name || card.dataset.object;

            this.ui.ask({
                message: "Turn " + (on ? "on" : "off") + " " + name + "?",
                askPassword: control === "password"
            }, function (answer) {
                if (!answer.confirmed) {
                    return;
                }

                body.Confirmed = true;

                if (control === "password") {
                    body.Password = answer.password;
                }

                sendIt();
            });

            return;
        }

        sendIt();
    };

    var exported = {ConfirmDialog: ConfirmDialog, EsphomeSwitches: EsphomeSwitches, reasonFrom: reasonFrom};

    if (typeof module !== "undefined" && module.exports) {
        module.exports = exported;
    }

    if (typeof document !== "undefined" && global.addEventListener) {
        new EsphomeSwitches(document).init();
    }
})(typeof window !== "undefined" ? window : globalThis);
