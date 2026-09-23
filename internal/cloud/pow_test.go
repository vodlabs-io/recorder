package cloud

import "testing"

func TestSolvePoWProducesAnAcceptedSolution(t *testing.T) {
	const difficulty = 12 // small enough to solve instantly in a test
	solution, err := solvePoW("test-nonce", difficulty)
	if err != nil {
		t.Fatalf("solvePoW: %v", err)
	}
	if !powMeetsDifficulty("test-nonce", solution, difficulty) {
		t.Fatal("the solution solvePoW returned should meet the difficulty it solved for")
	}
}

func TestSolvePoWSolutionIsBoundToItsNonce(t *testing.T) {
	const difficulty = 12
	solution, err := solvePoW("test-nonce", difficulty)
	if err != nil {
		t.Fatalf("solvePoW: %v", err)
	}
	if powMeetsDifficulty("a-different-nonce", solution, difficulty) {
		t.Fatal("a solution must not check out against a nonce it wasn't solved for")
	}
}

func TestPowMeetsDifficultyZeroAlwaysPasses(t *testing.T) {
	if !powMeetsDifficulty("any-nonce", "any-solution", 0) {
		t.Fatal("zero required bits means every hash qualifies")
	}
}
