// Asks the person at the page to confirm, and for a password if the switch needs one. It's a dialog of its own, not
// the browser's prompt(), because that shows what's typed. It's built from elements, never from markup, so nothing
// in a name can become one.
class ConfirmDialog {
    constructor(doc) {
        this.doc = doc;
    }

    // Resolves to {confirmed, password}. Cancelling, pressing Escape and closing it all give confirmed: false.
    ask = ({message, askPassword}) => new Promise((resolve) => {
        const doc = this.doc;
        const dialog = doc.createElement("dialog");
        dialog.className = "esphome-confirm";

        const form = doc.createElement("form");
        form.method = "dialog";

        const text = doc.createElement("p");
        text.className = "message";
        text.textContent = message;
        form.appendChild(text);

        let input = null;

        if (askPassword) {
            input = doc.createElement("input");
            input.type = "password";
            input.name = "password";
            input.className = "password";
            input.autocomplete = "off";
            input.required = true;
            input.setAttribute("aria-label", "Password");
            form.appendChild(input);
        }

        const buttons = doc.createElement("div");
        buttons.className = "buttons";

        const cancel = doc.createElement("button");
        cancel.type = "button";
        cancel.className = "cancel";
        cancel.textContent = "Cancel";
        cancel.addEventListener("click", () => dialog.close("cancel"));

        const confirm = doc.createElement("button");
        confirm.type = "submit";
        confirm.className = "confirm";
        confirm.value = "confirm";
        confirm.textContent = "Confirm";

        buttons.appendChild(cancel);
        buttons.appendChild(confirm);
        form.appendChild(buttons);
        dialog.appendChild(form);

        dialog.addEventListener("close", () => {
            const confirmed = dialog.returnValue === "confirm";
            const password = input ? input.value : "";

            // Don't leave what was typed in the page.
            if (input) {
                input.value = "";
            }

            dialog.remove();
            resolve({confirmed, password});
        });

        doc.body.appendChild(dialog);
        dialog.showModal();
        (input || confirm).focus();
    });
}

class EsphomeSwitches {
    constructor(doc, ui) {
        this.doc = doc;
        this.ui = ui || new ConfirmDialog(doc);
    }

    init = () => {
        const cards = this.doc.querySelectorAll("div.entity-esphome-switch");

        cards.forEach((card) => {
            card.querySelectorAll("button.switch-toggle").forEach((button) => {
                button.addEventListener("click", () => this.send(card, button));
            });
        });

        return cards.length > 0;
    }

    send = async (card, button) => {
        const errorElement = card.querySelector(".switch-error");
        const buttons = card.querySelectorAll("button.switch-toggle");
        errorElement.textContent = "";

        const on = button.dataset.on === "true";
        const control = button.dataset.control || "open";
        const body = {Device: card.dataset.device, Object: card.dataset.object, On: on};

        // The server decides what's allowed: this only asks, so that what it needs is there to be checked.
        if (control === "confirm" || control === "password") {
            const name = card.dataset.name || card.dataset.object;
            const answer = await this.ui.ask({
                message: "Turn " + (on ? "on" : "off") + " " + name + "?",
                askPassword: control === "password",
            });

            if (!answer.confirmed) {
                return;
            }

            body.Confirmed = true;

            if (control === "password") {
                body.Password = answer.password;
            }
        }

        buttons.forEach((b) => b.disabled = true);

        try {
            const response = await fetch("/plugins/esphome/switch", {
                method: "POST",
                headers: {"Content-Type": "application/json"},
                body: JSON.stringify(body),
            });

            if (!response.ok) {
                // Say so on the page: a click that silently does nothing is worse than an error. What the server
                // says is about the request ("wrong password", "the device is offline"), so it's worth showing.
                const reason = (await response.text()).replace(/^Failed to handle request: /, "").split("\n")[0].slice(0, 200);
                console.error("Failed to switch:", response.status, reason);
                errorElement.textContent = "Could not send the command (" + response.status + "): " + reason;
                buttons.forEach((b) => b.disabled = false);
                return;
            }

            // The request has been sent, not carried out: the device reports its new state a moment later, so
            // give it a second before the page asks for it.
            setTimeout(() => location.reload(), 1000);
        } catch (error) {
            console.error("Failed to switch:", error);
            errorElement.textContent = "Could not send the command: " + error;
            buttons.forEach((b) => b.disabled = false);
        }
    }
}

new EsphomeSwitches(document).init();
