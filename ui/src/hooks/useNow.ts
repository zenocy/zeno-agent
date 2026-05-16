import { useEffect, useState } from "react";
import dayjs from "dayjs";

export function useNow() {
  const [t, setT] = useState(() => dayjs());
  useEffect(() => {
    const id = setInterval(() => setT(dayjs()), 30_000);
    return () => clearInterval(id);
  }, []);
  return t;
}
