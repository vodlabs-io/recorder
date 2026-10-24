// Signs in with the token from a `vod web url` link (#token=…) or the form.
// The token is stripped from the address bar first, so it stays out of the
// history and anything that reads the URL.
"use strict";

(function () {
  const form = document.getElementById("form");
  const input = document.getElementById("token");
  const status = document.getElementById("status");
  const button = form.querySelector("button");

  async function signIn(token) {
    button.disabled = true;
    status.textContent = "";
    try {
      const res = await fetch("api/session", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: token.trim() }),
        credentials: "same-origin",
      });
      if (res.ok) {
        location.replace("./");
        return;
      }
      if (res.status === 429) {
        const wait = Number(res.headers.get("Retry-After")) || 900;
        status.textContent = `Too many failed sign-ins. Try again in ${Math.ceil(wait / 60)} min.`;
      } else if (res.status === 401) {
        status.textContent = "That token is not right, or it was rotated.";
      } else {
        status.textContent = `Sign-in failed (${res.status}).`;
      }
    } catch {
      status.textContent = "Could not reach the recorder.";
    }
    button.disabled = false;
  }

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    if (input.value.trim()) void signIn(input.value);
  });

  const params = new URLSearchParams(location.hash.slice(1));
  const token = params.get("token");
  if (token) {
    history.replaceState(null, "", location.pathname + location.search);
    void signIn(token);
  } else {
    fetch("api/session", { credentials: "same-origin" })
      .then((res) => {
        if (res.ok) location.replace("./");
      })
      .catch(() => {});
  }
})();
