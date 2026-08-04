FROM python:3.12-slim
RUN addgroup --system scos && adduser --system --ingroup scos scos && \
    pip install --no-cache-dir "psycopg[binary]>=3.2,<4"
WORKDIR /app
COPY main.py .
USER scos
CMD ["python3", "main.py"]
