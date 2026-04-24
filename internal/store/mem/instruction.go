package mem

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// UserInstructionStore is a thread-safe in-memory implementation of store.UserInstructionStore.
type UserInstructionStore struct {
	mu           sync.RWMutex
	instructions map[string]model.UserInstruction
}

// NewUserInstructionStore returns an empty in-memory instruction store.
func NewUserInstructionStore() *UserInstructionStore {
	return &UserInstructionStore{instructions: make(map[string]model.UserInstruction)}
}

func (s *UserInstructionStore) Create(_ context.Context, instruction model.UserInstruction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instructions[instruction.ID] = instruction
	return nil
}

func (s *UserInstructionStore) Get(_ context.Context, instructionID string) (model.UserInstruction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ins, ok := s.instructions[instructionID]
	if !ok {
		return model.UserInstruction{}, &store.ErrNotFound{Entity: "instruction", ID: instructionID}
	}
	return ins, nil
}

func (s *UserInstructionStore) List(_ context.Context, filter store.UserInstructionFilter) ([]model.UserInstruction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []model.UserInstruction
	for _, ins := range s.instructions {
		if filter.UserID != "" && ins.UserID != filter.UserID {
			continue
		}
		if filter.Active != nil && ins.Active != *filter.Active {
			continue
		}
		result = append(result, ins)
	}
	return result, nil
}

func (s *UserInstructionStore) Update(_ context.Context, instruction model.UserInstruction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instructions[instruction.ID]; !ok {
		return &store.ErrNotFound{Entity: "instruction", ID: instruction.ID}
	}
	s.instructions[instruction.ID] = instruction
	return nil
}

func (s *UserInstructionStore) Delete(_ context.Context, instructionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instructions[instructionID]; !ok {
		return &store.ErrNotFound{Entity: "instruction", ID: instructionID}
	}
	delete(s.instructions, instructionID)
	return nil
}
