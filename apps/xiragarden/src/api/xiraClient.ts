export const defaultXiraBaseURL =
  import.meta.env.VITE_XIRA_API_BASE_URL ?? "http://127.0.0.1:8089";

export const xiraGardenChannel = "xiragarden";

export type XiraStatus = {
  name: string;
  config_path: string;
  workspace: string;
  run_root: string;
  session_root: string;
  state_dir: string;
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
  schema_version?: number;
  run_id?: string;
  kind: string;
  time: string;
  source: string;
  source_detail?: {
    component?: string;
    name?: string;
  };
  scope?: {
    entrypoint_id?: string;
    channel?: string;
    account?: string;
    channel_app_id?: string;
    bot_id?: string;
    conversation_session_id?: string;
    agent_session_id?: string;
    run_id?: string;
    agent_id?: string;
    child_agent_id?: string;
    chat_id?: string;
    chat_type?: string;
    topic_id?: string;
    space_id?: string;
    space_type?: string;
    sender_id?: string;
    message_id?: string;
    reply_to_message_id?: string;
    reply_to_sender_id?: string;
    delegation_depth?: number;
  };
  correlation?: {
    trace_id?: string;
    parent_run_id?: string;
    child_run_id?: string;
    parent_event_id?: string;
    tool_call_id?: string;
  };
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
