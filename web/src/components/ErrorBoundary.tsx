import { Component, type ReactNode } from "react";

// ErrorBoundary is the top of the degradation chain (a sibling product's
// ErrorBoundary): a render-phase crash lands here instead of a white
// page, and retry remounts the subtree — data fetches below re-run on
// their own. State stays local; no logging surface exists yet.
export default class ErrorBoundary extends Component<
  { children: ReactNode },
  { error: Error | null }
> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    if (this.state.error) {
      return (
        <div className="boundary">
          <div className="boundary-title">页面加载出错</div>
          <div className="boundary-detail">{this.state.error.message}</div>
          <button
            className="ghost"
            onClick={() => this.setState({ error: null })}
          >
            重试
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
