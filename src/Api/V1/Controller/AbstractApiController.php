<?php

namespace App\Api\V1\Controller;

use App\Api\V1\DTO\ApiResponse;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\HttpFoundation\{JsonResponse, Response};

abstract class AbstractApiController extends Controller
{
    protected const DEFAULT_SERIALIZER_GROUPS = ['api_v1'];

    protected function createJsonResponse($data, array $groups = [], int $code = Response::HTTP_OK, string $message = null, string $status = ''): JsonResponse
    {
        return $this->json(new ApiResponse($data, $code, $message, $status), $code, [], [
            'groups' => array_merge(self::DEFAULT_SERIALIZER_GROUPS,$groups),
        ]);
    }
}