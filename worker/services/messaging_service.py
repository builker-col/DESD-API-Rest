import time
import pika
from config import Config

class MessagingService:
    def __init__(self):
        self.credentials = pika.PlainCredentials(Config.RABBITMQ_USER, Config.RABBITMQ_PASS)
        self.connection = None
        self.channel = None

        # Intentar conexión con reintentos y backoff exponencial simple
        max_attempts = 12  # ~1-2 minutos según backoff
        delay_seconds = 2
        for attempt in range(1, max_attempts + 1):
            try:
                params = pika.ConnectionParameters(
                    host=Config.RABBITMQ_HOST,
                    credentials=self.credentials,
                    heartbeat=600,
                    blocked_connection_timeout=300,
                    connection_attempts=1,
                    retry_delay=0,
                )
                self.connection = pika.BlockingConnection(params)
                self.channel = self.connection.channel()
                self.channel.queue_declare(queue=Config.QUEUE_NAME, durable=True)
                self.channel.basic_qos(prefetch_count=5)
                break
            except pika.exceptions.AMQPConnectionError as exc:
                wait_time = delay_seconds * min(8, 2 ** (attempt - 1))
                print(f"[MessagingService] No se pudo conectar a RabbitMQ (intento {attempt}/{max_attempts}): {exc}. Reintentando en {wait_time}s...")
                time.sleep(wait_time)

        if self.channel is None:
            raise RuntimeError("No fue posible conectar a RabbitMQ despues de varios intentos")
        
    def consume(self, callback):
        self.channel.basic_consume(queue=Config.QUEUE_NAME, on_message_callback=callback)
        print(f"Worker for {Config.QUEUE_NAME} is running...")
        self.channel.start_consuming()