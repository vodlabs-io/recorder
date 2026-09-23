import { useEffect, useRef } from "react"
import debounce from "lodash.debounce"

/**
 * Automatically runs `onSave` whenever a form field value changes and is different from the previous one.
 * 
 * @param value - The current field value (from react-hook-form's watch)
 * @param onSave - Called with the new value when it changes
 * @param delay - Debounce delay in ms
 */
export function useAutoSaveField<T>(value: T, onSave: (value: T) => void, delay = 300) {
  const prevValueRef = useRef(value)

  // Create debounced save function
  const debouncedSave = useRef(
    debounce((val: T) => {
      onSave(val)
    }, delay)
  ).current

  useEffect(() => {
    if (value !== prevValueRef.current) {
      prevValueRef.current = value
      debouncedSave(value)
    }
  }, [value, debouncedSave])
}
export default useAutoSaveField;