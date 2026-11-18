function isPageLifecycleRequest(request) {
  if (request.method() !== "POST") return false;
  const url = new URL(request.url());
  return url.origin === "http://magpie.test" &&
    ["/api/update/using", "/api/page-away"].includes(url.pathname);
}

module.exports = { isPageLifecycleRequest };
