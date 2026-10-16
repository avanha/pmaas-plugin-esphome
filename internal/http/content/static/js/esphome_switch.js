class EsphomeSwitches {
    constructor(doc) {
        this.doc = doc;
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
        buttons.forEach((b) => b.disabled = true);

        try {
            const response = await fetch("/plugins/esphome/switch", {
                method: "POST",
                headers: {"Content-Type": "application/json"},
                body: JSON.stringify({
                    Device: card.dataset.device,
                    Object: card.dataset.object,
                    On: button.dataset.on === "true",
                }),
            });

            if (!response.ok) {
                // Say so on the page: a click that silently does nothing is worse than an error.
                console.error("Failed to switch:", response.status, await response.text());
                errorElement.textContent = "Could not send the command (" + response.status + ")";
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
