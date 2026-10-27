// Optional enhancement: list actions update in place instead of reloading the
// page, so the scroll position and the keyboard stay put. Without JS the
// forms still work as normal POST + redirect.
(function () {
  async function refresh(response) {
    const doc = new DOMParser().parseFromString(await response.text(), "text/html");
    const fresh = doc.getElementById("list");
    const current = document.getElementById("list");
    if (!fresh || !current) {
      location.reload();
      return;
    }
    current.replaceWith(fresh);
  }

  document.addEventListener("submit", async (e) => {
    const form = e.target;
    if (form.dataset.confirm && !confirm(form.dataset.confirm)) {
      e.preventDefault();
      return;
    }
    if (form.dataset.busy) {
      // Regular submit (file uploads); just show that something is happening.
      const button = form.querySelector("button[type=submit]");
      button.disabled = true;
      button.textContent = form.dataset.busy;
      return;
    }
    if (!form.hasAttribute("data-ajax")) return;
    e.preventDefault();
    try {
      const body = new FormData(form);
      for (const input of form.querySelectorAll("input[type=file][data-resize]")) {
        if (input.files[0]) body.set(input.name, await shrinkImage(input.files[0]), "photo.jpg");
      }
      const res = await fetch(form.action, { method: "POST", body });
      if (res.redirected && new URL(res.url).pathname === "/login") {
        location.href = res.url;
        return;
      }
      if (!res.ok) throw new Error(res.status);
      await refresh(res);
      if (form.classList.contains("add")) {
        form.reset();
        form.querySelector("input").focus();
      }
    } catch {
      form.submit();
    }
  });

  // Edit / Cancel links swap the list in place.
  document.addEventListener("click", async (e) => {
    const link = e.target.closest("a[data-swap]");
    if (!link) return;
    e.preventDefault();
    try {
      const res = await fetch(link.href);
      if (!res.ok || res.redirected) throw new Error(res.status);
      await refresh(res);
      const input = document.querySelector("#list form.edit input");
      if (input) {
        input.focus();
        input.setSelectionRange(input.value.length, input.value.length);
      }
    } catch {
      location.href = link.href;
    }
  });

  // ---- Item photos ----

  // Phone photos are several MB; send a ~1280px JPEG instead. Re-encoding
  // also drops metadata such as GPS location. Falls back to the original.
  async function shrinkImage(file, max = 1280) {
    try {
      const bitmap = await createImageBitmap(file);
      const scale = Math.min(1, max / Math.max(bitmap.width, bitmap.height));
      const canvas = document.createElement("canvas");
      canvas.width = Math.round(bitmap.width * scale);
      canvas.height = Math.round(bitmap.height * scale);
      canvas.getContext("2d").drawImage(bitmap, 0, 0, canvas.width, canvas.height);
      bitmap.close?.();
      const blob = await new Promise((resolve) => canvas.toBlob(resolve, "image/jpeg", 0.82));
      return blob || file;
    } catch {
      return file;
    }
  }

  // Preview the chosen photo in the edit form.
  document.addEventListener("change", (e) => {
    const input = e.target;
    if (!input.matches?.("input[type=file][data-resize]") || !input.files[0]) return;
    const preview = input.form.querySelector("[data-photo-preview]");
    if (preview) {
      preview.src = URL.createObjectURL(input.files[0]);
      preview.hidden = false;
    }
    input.form.querySelector("[data-photo-label]").textContent = "Change photo";
    const remove = input.form.querySelector("[name=remove_photo]");
    if (remove) remove.checked = false;
  });

  // Tap a thumbnail to see the photo large.
  document.addEventListener("click", (e) => {
    const link = e.target.closest("a[data-lightbox]");
    if (!link || typeof HTMLDialogElement === "undefined") return;
    e.preventDefault();
    let dialog = document.getElementById("lightbox");
    if (!dialog) {
      dialog = document.createElement("dialog");
      dialog.id = "lightbox";
      dialog.className = "lightbox";
      dialog.innerHTML = '<img alt=""><button type="button" aria-label="Close">×</button>';
      dialog.addEventListener("click", () => dialog.close()); // tap anywhere to close
      document.body.append(dialog);
    }
    dialog.querySelector("img").src = link.href;
    dialog.querySelector("img").alt = link.getAttribute("aria-label") || "";
    dialog.showModal();
  });

  // ---- Push notifications (turned on per device in the Family tab) ----
  const pushSupported = "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  const swRegistered = pushSupported ? navigator.serviceWorker.register("/sw.js") : null;
  swRegistered?.catch(() => {});

  const pushButton = document.querySelector("[data-push]");
  if (pushButton) setUpPushButton(pushButton, document.querySelector("[data-push-status]"));

  async function setUpPushButton(button, status) {
    if (!pushSupported) {
      const iPhone = /iPhone|iPad|iPod/.test(navigator.userAgent);
      status.textContent = iPhone
        ? "On iPhone: tap Share → Add to Home Screen, open the app from the Home Screen icon, then come back here."
        : "This browser doesn't support notifications.";
      return;
    }
    let reg;
    try {
      await swRegistered;
      reg = await navigator.serviceWorker.ready;
    } catch {
      status.textContent = "Notifications aren't available in this browser.";
      return;
    }
    let subscribed = false;

    async function show() {
      subscribed = !!(await reg.pushManager.getSubscription());
      button.hidden = false;
      button.textContent = subscribed ? "Turn off on this device" : "Turn on notifications";
      button.className = subscribed ? "secondary push-btn" : "primary push-btn";
      status.textContent = subscribed
        ? "Notifications are on for this device."
        : Notification.permission === "denied"
          ? "Notifications are blocked for this site. Allow them in the phone's or browser's settings."
          : "Notifications are off on this device.";
    }

    const post = (url, body) =>
      fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) })
        .then((res) => { if (!res.ok) throw new Error("server said " + res.status); });

    button.addEventListener("click", async () => {
      button.disabled = true;
      try {
        if (subscribed) {
          const sub = await reg.pushManager.getSubscription();
          if (sub) {
            await post("/push/unsubscribe", { endpoint: sub.endpoint });
            await sub.unsubscribe();
          }
        } else {
          // Ask first, straight from the tap (iPhone requires that).
          if ((await Notification.requestPermission()) !== "granted") return;
          const key = await (await fetch("/push/key")).text();
          const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: base64UrlToBytes(key) });
          await post("/push/subscribe", sub.toJSON());
        }
        location.reload(); // refresh the 🔔 next to members
      } catch (err) {
        status.textContent = "Couldn't change notifications (" + err.message + "). Please try again.";
      } finally {
        button.disabled = false;
        if (!status.textContent.startsWith("Couldn't")) show();
      }
    });

    show();
  }

  function base64UrlToBytes(s) {
    const b64 = (s + "=".repeat((4 - (s.length % 4)) % 4)).replace(/-/g, "+").replace(/_/g, "/");
    return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  }

  // Pick up changes other family members made while the tab was in the background.
  document.addEventListener("visibilitychange", async () => {
    if (document.visibilityState !== "visible" || !document.getElementById("list")) return;
    if (document.querySelector("#list form.edit")) return; // don't discard an edit in progress
    try {
      const res = await fetch(location.pathname);
      if (res.ok && !res.redirected) await refresh(res);
    } catch {}
  });
})();
