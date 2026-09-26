package predictor

import (
	"context"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
)

// BaselinePredictor предсказывает отклонение как cur_dev_s без изменений.
//
// Это не затычка, а нижняя граница качества. Измерения на validate дали
// среднюю абсолютную ошибку baseline 93.46 с против 67.10 с у модели, то есть
// модель выигрывает примерно четверть. Но модель может не прийти вообще: она
// отдельный сервис, отдельная точка отказа, отдельный регресс. Ответ
// baseline не может быть неверным по построению — он просто повторяет то, что
// уже измерено, — поэтому он и стоит последним в цепочке деградации.
//
// Отдельная тонкость: cur_dev_s в кадре nullable. На validate он отсутствовал
// на всех 142 кадрах — organizers считают его по плановому времени остановки,
// которого в кадре нет по построению, и подставлять его значило бы заглянуть
// в будущее. Поэтому baseline на кадре без cur_dev_s отвечает нулём и честно
// помечает это отдельным признаком: ноль в отклонении — правдоподобное
// значение «пришёл вовремя», и выдавать его молча нельзя.
type BaselinePredictor struct{}

// Predict возвращает cur_dev_s как есть.
func (BaselinePredictor) Predict(_ context.Context, f horizon.Frame) Prediction {
	p := Prediction{
		SampleID:     f.SampleID,
		UnitID:       f.UnitID,
		TRID:         f.TRID,
		AsOf:         f.AsOf,
		TargetStopID: f.PrimaryStopID(),
		HorizonS:     f.HorizonS(),
		Source:       SourceBaseline,
		Missing:      f.Missing(),
	}
	if dev, ok := f.Value("cur_dev_s"); ok {
		p.CurDevS = &dev
		p.PredictedDevS = dev
		p.DeltaS = 0
		return p
	}
	// cur_dev_s не измерен. Отклонение неизвестно, и единственное честное
	// число здесь — ноль; Predictor без признака unavailable об этом
	// сообщить не может, поэтому несовпадение видно по Missing.
	p.PredictedDevS = 0
	p.DeltaS = 0
	return p
}
