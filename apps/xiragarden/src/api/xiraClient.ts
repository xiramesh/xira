export const defaultXiraBaseURL =
  import.meta.env.VITE_XIRA_API_BASE_URL ?? "http://127.0.0.1:8089";

export const xiraGardenChannel = "xiragarden";

export type XiraStatus = {
  name: string;
  config_path: string;
  workspace: string;
  run_root: string;
  session_root: string;
  state_root: string;
  agents: number;
  entrypoints: number;
  default_agent: string;
  profile_source: string;
};

export type XiraGardenMessageRequest = {
  message: string;
  agent_id?: string;
  entrypoint_id?: string;
  session_id?: string;
  user_id?: string;
  metadata?: Record<string, string>;
};

export type XiraRuntimeEvent = {
  id: string;
  run_id?: string;
  kind: string;
  time: string;
  source: string;
  severity?: string;
  message?: string;
  payload?: Record<string, unknown>;
};

export type XiraTurnResponse = {
  run_id: string;
  agent_id: string;
  entrypoint_id?: string;
  session_id: string;
  message: string;
  final_response: string;
  status: string;
  started_at: string;
  ended_at: string;
  events?: XiraRuntimeEvent[];
  audit_events?: unknown[];
  metadata?: Record<string, string>;
};

export async function getXiraStatus(baseURL = defaultXiraBaseURL): Promise<XiraStatus> {
  const response = await fetch(`${baseURL}/api/v1/status`);
  if (!response.ok) {
    throw new Error(`status request failed: ${response.status}`);
  }
  return response.json() as Promise<XiraStatus>;
}

export async function sendXiraGardenMessage(
  request: XiraGardenMessageRequest,
  baseURL = defaultXiraBaseURL
): Promise<XiraTurnResponse> {
  const response = await fetch(`${baseURL}/api/v1/channels/${xiraGardenChannel}/messages`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(request)
  });
  if (!response.ok) {
    throw new Error(`xiragarden message request failed: ${response.status}`);
  }
  return response.json() as Promise<XiraTurnResponse>;
}

export function openXiraGardenEvents(baseURL = defaultXiraBaseURL): WebSocket {
  const url = new URL(`/api/v1/channels/${xiraGardenChannel}/events`, baseURL);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return new WebSocket(url);
}
