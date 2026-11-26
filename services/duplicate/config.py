import os

class Config:    
    DEBUG = False
    # RabbitMQ configuration
    RABBITMQ_HOST = os.getenv('RABBITMQ_HOST', 'rabbitmq')
    RABBITMQ_USER = os.getenv('RABBITMQ_USER', 'user')
    RABBITMQ_PASS = os.getenv('RABBITMQ_PASS', 'password')
    QUEUE_NAME = os.getenv('QUEUE_NAME', 'queue_dude')

    # PostgreSQL configuration
    POSTGRES_URI = os.getenv('POSTGRES_URI', 'postgresql://user:password@postgres:5432/desd')