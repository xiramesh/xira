import React from "react";
import { createRoot } from "react-dom/client";

import { XiraGardenApp } from "./xiragarden-app";
import "./styles/app.css";

createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <XiraGardenApp />
  </React.StrictMode>
);
