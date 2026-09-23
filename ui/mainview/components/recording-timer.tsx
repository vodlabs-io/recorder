import React, { useEffect, useState } from 'react'

interface TimerProps {
  // Unix seconds the recording/stream started. Ticks locally every second.
  startUnixSec: number
}

// formatElapsed renders a second count in the timer's H:MM:SS form.
export function formatElapsed(totalSeconds: number): string {
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60

  return `${hours}:${minutes.toString().padStart(2, '0')}:${seconds
    .toString()
    .padStart(2, '0')}`
}

const RecordingTimer: React.FC<TimerProps> = ({ startUnixSec }) => {
  const startMs = startUnixSec * 1000
  const [elapsedSeconds, setElapsedSeconds] = useState<number>(
    Math.max(0, Math.floor((Date.now() - startMs) / 1000))
  )

  useEffect(() => {
    const interval = setInterval(() => {
      setElapsedSeconds(Math.max(0, Math.floor((Date.now() - startMs) / 1000)))
    }, 1000)
    return () => clearInterval(interval)
  }, [startMs])

  return <span>{formatElapsed(elapsedSeconds)}</span>
}

export default RecordingTimer
