import { invoke } from "@tauri-apps/api/core";
import type {
  CodebuddyFetchModelsResult,
  CodebuddyLocalAccessSavePayload,
  CodebuddyLocalAccessState,
  CodebuddyLocalAccessTestResult,
  CodebuddyProbeResult,
} from "../types/codebuddyLocalAccess";

export async function getCodebuddyLocalAccessState(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_get_state");
}

export async function saveCodebuddyLocalAccess(
  payload: CodebuddyLocalAccessSavePayload,
): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_save", {
    ...(payload as Record<string, unknown>),
  });
}

export async function startCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_start");
}

export async function stopCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_stop");
}

export async function restartCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_restart");
}

export async function rotateCodebuddyLocalAccessApiKey(): Promise<CodebuddyLocalAccessState> {
  return await invoke("codebuddy_local_access_rotate_api_key");
}

export async function testCodebuddyLocalAccess(): Promise<CodebuddyLocalAccessTestResult> {
  return await invoke("codebuddy_local_access_test");
}

export async function getCodebuddyLocalAccessDefaultModels(): Promise<string[]> {
  return await invoke("codebuddy_local_access_default_models");
}

/** 直连上游企业模型接口，拉取所选账号真实可用的模型。 */
export async function fetchCodebuddyModels(): Promise<CodebuddyFetchModelsResult> {
  return await invoke("codebuddy_local_access_fetch_models");
}

/** 经本地服务发起一次真实对话，探测模型是否响应。 */
export async function probeCodebuddyChat(
  model: string,
  prompt?: string,
): Promise<CodebuddyProbeResult> {
  return await invoke("codebuddy_local_access_probe_chat", { model, prompt: prompt ?? null });
}
