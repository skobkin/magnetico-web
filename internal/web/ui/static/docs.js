/* Initialises the vendored Swagger UI on the public API documentation pages.
 * First-party, no inline script (the pages carry the specification URL in a
 * data attribute). Nothing is ever stored or sent anywhere but this origin:
 * the validator service and telemetry are off, authorization is not
 * persisted, requests omit cookies, and "Try it out" is limited to GET. The
 * user types an API token into the Authorize dialog by hand; the page never
 * contains one. */
(function () {
  "use strict";
  var el = document.getElementById("swagger-ui");
  if (!el || typeof window.SwaggerUIBundle !== "function") { return; }
  window.SwaggerUIBundle({
    url: el.getAttribute("data-spec"),
    dom_id: "#swagger-ui",
    deepLinking: false,
    presets: [window.SwaggerUIBundle.presets.apis],
    layout: "BaseLayout",
    validatorUrl: null,
    persistAuthorization: false,
    supportedSubmitMethods: ["get"],
    tryItOutEnabled: false,
    withCredentials: false,
    displayRequestDuration: true,
    docExpansion: "list",
    defaultModelsExpandDepth: 1,
    requestInterceptor: function (req) {
      req.credentials = "omit";
      return req;
    }
  });
})();
