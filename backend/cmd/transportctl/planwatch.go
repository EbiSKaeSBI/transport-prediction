package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/pipeline"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
)

// planWatcher перечитывает план-график и привязку, когда файлы меняются, и
// подменяет их на лету.
//
// Зачем это нужно. План-график эмулятора строится из записи телеметрии, то
// есть описывает дорогу там, где машина была. Пока машина едет по своей
// дороге, запись стареет, и план уезжает от неё: через минуты привязка к
// маршруту теряется, риск-сегменты исчезают, а перезапуск конвейера роняет
// накопленную телеметрию и разрывает поток. Перечитывание файла решает это без
// остановки сервиса.
//
// Реализация — опрос по хешу содержимого, а не fsnotify: зависимость ради двух
// файлов не стоит, а опрос не требует платформенных прав, которых у Docker-образа
// может не быть.
type planWatcher struct {
	planPath    string
	bindingPath string
	holder      *schedule.Holder
	pipe        *pipeline.Pipeline
	logger      *slog.Logger
	every       time.Duration

	// Хеши файлов, которые уже подменены. Пустой хеш означает «ещё ни разу».
	planHash    [sha256.Size]byte
	bindingHash [sha256.Size]byte
	planSeen    bool
	bindingSeen bool
	// lastErr — текст последней неудачи, чтобы не сыпать одинаковую ошибку
	// каждую секунду, пока файл дописывается.
	lastErr string
	reloads int
}

// newPlanWatcher создаёт наблюдатель и запоминает хеши уже загруженных файлов,
// чтобы первая же проверка не перечитывала план заново.
func newPlanWatcher(planPath, bindingPath string, holder *schedule.Holder,
	pipe *pipeline.Pipeline, logger *slog.Logger, every time.Duration,
) *planWatcher {
	if every <= 0 {
		every = time.Second
	}
	w := &planWatcher{
		planPath:    planPath,
		bindingPath: bindingPath,
		holder:      holder,
		pipe:        pipe,
		logger:      logger,
		every:       every,
	}
	w.planHash, _ = fileHash(planPath)
	w.bindingHash, _ = fileHash(bindingPath)
	w.planSeen = w.planHash != [sha256.Size]byte{}
	w.bindingSeen = w.bindingHash != [sha256.Size]byte{}
	return w
}

func fileHash(path string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	return sha256.Sum256(data), nil
}

// Run опрашивает файлы до отмены контекста.
func (w *planWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("наблюдение за планом остановлено",
				"перепривязок", w.reloads)
			return
		case <-ticker.C:
			w.step()
		}
	}
}

// step — одна проверка. Ошибка чтения или разбора не трогает текущий план:
// перепривязка, которая не удалась, должна оставить сервис работающим на
// прежнем расписании, а не без него.
func (w *planWatcher) step() {
	planHash, err := fileHash(w.planPath)
	if err != nil {
		w.noteErr(err)
		return
	}
	bindingHash, err := fileHash(w.bindingPath)
	if err != nil {
		w.noteErr(err)
		return
	}
	planChanged := !w.planSeen || planHash != w.planHash
	bindingChanged := !w.bindingSeen || bindingHash != w.bindingHash
	if !planChanged && !bindingChanged {
		w.lastErr = ""
		return
	}
	plan, err := schedule.LoadFile(w.planPath)
	if err != nil {
		w.noteErr(fmt.Errorf("план %s: %w", w.planPath, err))
		return
	}
	binding, err := schedule.LoadBindingFile(w.bindingPath)
	if err != nil {
		w.noteErr(fmt.Errorf("привязка %s: %w", w.bindingPath, err))
		return
	}
	w.holder.Swap(plan, binding)
	// Новый план описывает ту же остановку другими координатами и временем, а
	// пара «машина, остановка» формально не меняется. Без сброса конвейер
	// молчал бы до ухода машины с участка — то есть ровно до того момента,
	// ради которого план переписывали.
	if w.pipe != nil {
		w.pipe.ForgetTargets()
	}
	w.planHash, w.planSeen = planHash, true
	w.bindingHash, w.bindingSeen = bindingHash, true
	w.reloads++
	w.lastErr = ""
	w.logger.Info("план перепривязан",
		"остановок", plan.StopsCount(), "единиц", binding.Len(),
		"перепривязок", w.reloads, "ошибок_привязки", binding.ConflictsCount())
}

// noteErr пишет ошибку, только если она новая: файл, который дописывается,
// даёт одно и то же сообщение каждую секунду, и молчание после первого раза
// сбивало бы с толку.
func (w *planWatcher) noteErr(err error) {
	if errors.Is(err, os.ErrNotExist) {
		if w.lastErr == "нет файла" {
			return
		}
		w.lastErr = "нет файла"
		w.logger.Warn("план временно недоступен, работаю на прежнем расписании",
			"err", err)
		return
	}
	msg := err.Error()
	if msg == w.lastErr {
		return
	}
	w.lastErr = msg
	w.logger.Warn("план не перечитан, работаю на прежнем расписании", "err", err)
}
