// Window/chrome helpers over the runtime bridge `mygo-runtime` exposes.
// Typed services and events come from the generated client: ./mygo.ts.

declare global {
  interface Window {
    mygo?: {
      platform: string;
      window: {
        minimize(): Promise<void>;
        maximize(): Promise<void>;
        unmaximize(): Promise<void>;
        toggleMaximize(): Promise<void>;
        isMaximized(): Promise<boolean>;
        toggleFullScreen(): Promise<void>;
        close(): Promise<void>;
        setTitle(title: string): Promise<void>;
      };
    };
  }
}

export const platform = () => window.mygo?.platform ?? "darwin";
export const winCtl = () => window.mygo?.window;
