import { useEffect, useRef, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { startPair, type PairFrame } from "../api/useWhatsApp";

interface WhatsAppPairModalProps {
  onClose: () => void;
  onPaired: () => void;
}

// WhatsAppPairModal opens an SSE stream against /api/whatsapp/pair and
// renders the rolling QR codes whatsmeow emits (one fresh code every
// ~30s). The user scans with their phone's WhatsApp → Linked Devices
// flow; on success the modal closes and the parent refetches status.
//
// Cancel button aborts the stream so a forgotten modal doesn't burn
// the server-side timeout window.
export function WhatsAppPairModal({ onClose, onPaired }: WhatsAppPairModalProps) {
  const [code, setCode] = useState<string | null>(null);
  const [phase, setPhase] = useState<"connecting" | "waiting" | "paired" | "error">("connecting");
  const [error, setError] = useState<string | null>(null);

  // Keep the latest onPaired in a ref so the SSE-opening effect can
  // call it without taking onPaired as a dependency. SettingsPanel's
  // 5s status refetch re-renders this component every cycle with a
  // freshly-bound onPaired closure; if we declared it as a useEffect
  // dep, the effect would re-run every 5s — tearing down the SSE,
  // pre-empting the in-flight pair, and never letting the user scan
  // the QR.
  const onPairedRef = useRef(onPaired);
  onPairedRef.current = onPaired;

  useEffect(() => {
    const cancel = startPair(
      (frame: PairFrame) => {
        switch (frame.event) {
          case "code":
            if (frame.code) {
              setCode(frame.code);
              setPhase("waiting");
            }
            break;
          case "success":
            setPhase("paired");
            // Brief celebratory pause so the user sees the success
            // state before the modal vanishes.
            window.setTimeout(() => onPairedRef.current(), 800);
            break;
          case "timeout":
            setError("Pairing timed out. Close and try again.");
            setPhase("error");
            break;
          case "error":
            setError(frame.error ?? "Pairing failed.");
            setPhase("error");
            break;
          case "closed":
            // Belt-and-braces for older server builds that emit `closed`
            // when whatsmeow's qrchan closes without a terminal event.
            // Newer servers translate this to `error` with a hint, but
            // we still need a UI arm so the modal doesn't sit on a stale
            // QR forever.
            setError("Pairing connection ended unexpectedly. Try again.");
            setPhase("error");
            break;
        }
      },
      (err) => {
        setError(err.message);
        setPhase("error");
      },
    );
    return () => cancel();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
      <div className="bg-bg-card border border-line rounded-z-md p-6 max-w-md w-full mx-4">
        <header className="mb-4">
          <h2 className="font-display font-[500] text-[18px] text-ink mb-1">
            Link WhatsApp
          </h2>
          <p className="text-[12px] text-ink-5">
            On your phone open WhatsApp → Settings → Linked Devices → Link a Device, then scan this code.
          </p>
        </header>

        <div className="flex justify-center my-6 min-h-[256px] items-center">
          {phase === "connecting" && (
            <div className="font-mono text-[12px] text-ink-5">Connecting…</div>
          )}
          {phase === "waiting" && code && (
            <div className="bg-white p-4 rounded-z-sm">
              <QRCodeSVG value={code} size={224} />
            </div>
          )}
          {phase === "paired" && (
            <div className="font-mono text-[14px] text-good">Paired.</div>
          )}
          {phase === "error" && (
            <div className="font-mono text-[12px] text-crit text-center px-4">
              {error}
            </div>
          )}
        </div>

        <div className="flex justify-end gap-2 pt-3 border-t border-line">
          <button
            type="button"
            onClick={onClose}
            className="h-[28px] px-3 rounded-z-sm text-[12px] font-[500] border border-line text-ink-5 hover:text-ink"
          >
            {phase === "paired" ? "Done" : "Cancel"}
          </button>
        </div>
      </div>
    </div>
  );
}
